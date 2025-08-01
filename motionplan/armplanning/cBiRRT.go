//go:build !windows && !no_cgo

package armplanning

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"go.viam.com/utils"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/ik"
	"go.viam.com/rdk/referenceframe"
)

const (
	// Maximum number of iterations that constrainNear will run before exiting nil.
	// Typically it will solve in the first five iterations, or not at all.
	maxNearIter = 20

	// Maximum number of iterations that constrainedExtend will run before exiting.
	maxExtendIter = 5000
)

// cBiRRTMotionPlanner an object able to solve constrained paths around obstacles to some goal for a given referenceframe.
// It uses the Constrained Bidirctional Rapidly-expanding Random Tree algorithm, Berenson et al 2009
// https://ieeexplore.ieee.org/document/5152399/
type cBiRRTMotionPlanner struct {
	*planner
	fastGradDescent ik.Solver
	algOpts         *cbirrtOptions
}

// newCBiRRTMotionPlannerWithSeed creates a cBiRRTMotionPlanner object with a user specified random seed.
func newCBiRRTMotionPlanner(
	fs *referenceframe.FrameSystem,
	seed *rand.Rand,
	logger logging.Logger,
	opt *PlannerOptions,
	constraintHandler *ConstraintHandler,
	chains *motionChains,
) (motionPlanner, error) {
	if opt == nil {
		return nil, errNoPlannerOptions
	}
	mp, err := newPlanner(fs, seed, logger, opt, constraintHandler, chains)
	if err != nil {
		return nil, err
	}
	// nlopt should try only once
	nlopt, err := ik.CreateNloptSolver(mp.lfs.dof, logger, 1, true, true)
	if err != nil {
		return nil, err
	}
	algOpts := opt.PlanningAlgorithmSettings.CBirrtOpts
	if algOpts == nil {
		algOpts = &cbirrtOptions{
			SolutionsToSeed: defaultSolutionsToSeed,
		}
	}
	algOpts.qstep = getFrameSteps(mp.lfs, defaultFrameStep)
	return &cBiRRTMotionPlanner{
		planner:         mp,
		fastGradDescent: nlopt,
		algOpts:         algOpts,
	}, nil
}

func (mp *cBiRRTMotionPlanner) plan(ctx context.Context, seed, goal *PlanState) ([]node, error) {
	solutionChan := make(chan *rrtSolution, 1)
	initMaps := initRRTSolutions(ctx, atomicWaypoint{mp: mp, startState: seed, goalState: goal})
	if initMaps.err != nil {
		return nil, initMaps.err
	}
	if initMaps.steps != nil {
		return initMaps.steps, nil
	}
	utils.PanicCapturingGo(func() {
		mp.rrtBackgroundRunner(ctx, &rrtParallelPlannerShared{initMaps.maps, nil, solutionChan})
	})
	solution := <-solutionChan
	if solution.err != nil {
		return nil, solution.err
	}
	return solution.steps, nil
}

// rrtBackgroundRunner will execute the plan. Plan() will call rrtBackgroundRunner in a separate thread and wait for results.
// Separating this allows other things to call rrtBackgroundRunner in parallel allowing the thread-agnostic Plan to be accessible.
func (mp *cBiRRTMotionPlanner) rrtBackgroundRunner(
	ctx context.Context,
	rrt *rrtParallelPlannerShared,
) {
	defer close(rrt.solutionChan)
	mp.logger.CDebugf(ctx, "starting cbirrt with start map len %d and goal map len %d\n", len(rrt.maps.startMap), len(rrt.maps.goalMap))

	// setup planner options
	if mp.planOpts == nil {
		rrt.solutionChan <- &rrtSolution{err: errNoPlannerOptions}
		return
	}
	// initialize maps
	// TODO(rb) package neighborManager better
	nm1 := &neighborManager{nCPU: mp.planOpts.NumThreads}
	nm2 := &neighborManager{nCPU: mp.planOpts.NumThreads}
	nmContext, cancel := context.WithCancel(ctx)
	defer cancel()
	mp.start = time.Now()

	var seed referenceframe.FrameSystemInputs
	// Pick a random (first in map) seed node to create the first interp node
	for sNode, parent := range rrt.maps.startMap {
		if parent == nil {
			seed = sNode.Q()
			break
		}
	}
	mp.logger.CDebugf(ctx, "goal node: %v\n", rrt.maps.optNode.Q())
	for n := range rrt.maps.startMap {
		mp.logger.CDebugf(ctx, "start node: %v\n", n.Q())
		break
	}
	mp.logger.Debug("DOF", mp.lfs.dof)
	interpConfig, err := referenceframe.InterpolateFS(mp.fs, seed, rrt.maps.optNode.Q(), 0.5)
	if err != nil {
		rrt.solutionChan <- &rrtSolution{err: err}
		return
	}
	target := newConfigurationNode(interpConfig)

	map1, map2 := rrt.maps.startMap, rrt.maps.goalMap

	m1chan := make(chan node, 1)
	m2chan := make(chan node, 1)
	defer close(m1chan)
	defer close(m2chan)

	for i := 0; i < mp.planOpts.PlanIter; i++ {
		select {
		case <-ctx.Done():
			mp.logger.CDebugf(ctx, "CBiRRT timed out after %d iterations", i)
			rrt.solutionChan <- &rrtSolution{err: fmt.Errorf("cbirrt timeout %w", ctx.Err()), maps: rrt.maps}
			return
		default:
		}
		if i > 0 && i%100 == 0 {
			mp.logger.CDebugf(ctx, "CBiRRT planner iteration %d", i)
		}

		tryExtend := func(target node) (node, node, error) {
			// attempt to extend maps 1 and 2 towards the target
			utils.PanicCapturingGo(func() {
				m1chan <- nm1.nearestNeighbor(nmContext, target, map1, nodeConfigurationDistanceFunc)
			})
			utils.PanicCapturingGo(func() {
				m2chan <- nm2.nearestNeighbor(nmContext, target, map2, nodeConfigurationDistanceFunc)
			})
			nearest1 := <-m1chan
			nearest2 := <-m2chan
			// If ctx is done, nearest neighbors will be invalid and we want to return immediately
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			default:
			}

			//nolint: gosec
			rseed1 := rand.New(rand.NewSource(int64(mp.randseed.Int())))
			//nolint: gosec
			rseed2 := rand.New(rand.NewSource(int64(mp.randseed.Int())))

			utils.PanicCapturingGo(func() {
				mp.constrainedExtend(ctx, rseed1, map1, nearest1, target, m1chan)
			})
			utils.PanicCapturingGo(func() {
				mp.constrainedExtend(ctx, rseed2, map2, nearest2, target, m2chan)
			})
			map1reached := <-m1chan
			map2reached := <-m2chan

			map1reached.SetCorner(true)
			map2reached.SetCorner(true)

			return map1reached, map2reached, nil
		}

		map1reached, map2reached, err := tryExtend(target)
		if err != nil {
			rrt.solutionChan <- &rrtSolution{err: err, maps: rrt.maps}
			return
		}

		reachedDelta := mp.configurationDistanceFunc(
			&motionplan.SegmentFS{
				StartConfiguration: map1reached.Q(),
				EndConfiguration:   map2reached.Q(),
			},
		)

		// Second iteration; extend maps 1 and 2 towards the halfway point between where they reached
		if reachedDelta > mp.planOpts.InputIdentDist {
			targetConf, err := referenceframe.InterpolateFS(mp.fs, map1reached.Q(), map2reached.Q(), 0.5)
			if err != nil {
				rrt.solutionChan <- &rrtSolution{err: err, maps: rrt.maps}
				return
			}
			target = newConfigurationNode(targetConf)
			map1reached, map2reached, err = tryExtend(target)
			if err != nil {
				rrt.solutionChan <- &rrtSolution{err: err, maps: rrt.maps}
				return
			}
			reachedDelta = mp.configurationDistanceFunc(&motionplan.SegmentFS{
				StartConfiguration: map1reached.Q(),
				EndConfiguration:   map2reached.Q(),
			})
		}

		// Solved!
		if reachedDelta <= mp.planOpts.InputIdentDist {
			mp.logger.CDebugf(ctx, "CBiRRT found solution after %d iterations", i)
			cancel()
			path := extractPath(rrt.maps.startMap, rrt.maps.goalMap, &nodePair{map1reached, map2reached}, true)
			rrt.solutionChan <- &rrtSolution{steps: path, maps: rrt.maps}
			return
		}

		// sample near map 1 and switch which map is which to keep adding to them even
		target, err = mp.sample(map1reached, i)
		if err != nil {
			rrt.solutionChan <- &rrtSolution{err: err, maps: rrt.maps}
			return
		}
		map1, map2 = map2, map1
	}
	rrt.solutionChan <- &rrtSolution{err: errPlannerFailed, maps: rrt.maps}
}

// constrainedExtend will try to extend the map towards the target while meeting constraints along the way. It will
// return the closest solution to the target that it reaches, which may or may not actually be the target.
func (mp *cBiRRTMotionPlanner) constrainedExtend(
	ctx context.Context,
	randseed *rand.Rand,
	rrtMap map[node]node,
	near, target node,
	mchan chan node,
) {
	// Allow qstep to be doubled as a means to escape from configurations which gradient descend to their seed
	deepCopyQstep := func() map[string][]float64 {
		qstep := map[string][]float64{}
		for fName, fStep := range mp.algOpts.qstep {
			newStep := make([]float64, len(fStep))
			copy(newStep, fStep)
			qstep[fName] = newStep
		}
		return qstep
	}
	qstep := deepCopyQstep()
	doubled := false

	oldNear := near
	// This should iterate until one of the following conditions:
	// 1) we have reached the target
	// 2) the request is cancelled/times out
	// 3) we are no longer approaching the target and our "best" node is further away than the previous best
	// 4) further iterations change our best node by close-to-zero amounts
	// 5) we have iterated more than maxExtendIter times
	for i := 0; i < maxExtendIter; i++ {
		select {
		case <-ctx.Done():
			mchan <- oldNear
			return
		default:
		}
		configDistMetric := mp.configurationDistanceFunc
		dist := configDistMetric(
			&motionplan.SegmentFS{StartConfiguration: near.Q(), EndConfiguration: target.Q()})
		oldDist := configDistMetric(
			&motionplan.SegmentFS{StartConfiguration: oldNear.Q(), EndConfiguration: target.Q()})

		switch {
		case dist < mp.planOpts.InputIdentDist:
			mchan <- near
			return
		case dist > oldDist:
			mchan <- oldNear
			return
		}

		oldNear = near

		newNear := fixedStepInterpolation(near, target, mp.algOpts.qstep)
		// Check whether newNear meets constraints, and if not, update it to a configuration that does meet constraints (or nil)
		newNear = mp.constrainNear(ctx, randseed, oldNear.Q(), newNear)

		if newNear != nil {
			nearDist := mp.configurationDistanceFunc(
				&motionplan.SegmentFS{StartConfiguration: oldNear.Q(), EndConfiguration: newNear})

			if nearDist < math.Pow(mp.planOpts.InputIdentDist, 3) {
				if !doubled {
					doubled = true
					// Check if doubling qstep will allow escape from the identical configuration
					// If not, we terminate and return.
					// If so, qstep will be reset to its original value after the rescue.
					for f, frameQ := range qstep {
						for i, q := range frameQ {
							qstep[f][i] = q * 2.0
						}
					}
					continue
				}
				// We've arrived back at very nearly the same configuration again; stop solving and send back oldNear.
				// Do not add the near-identical configuration to the RRT map
				mchan <- oldNear
				return
			}
			if doubled {
				qstep = deepCopyQstep()
				doubled = false
			}
			// constrainNear will ensure path between oldNear and newNear satisfies constraints along the way
			near = &basicNode{q: newNear}
			rrtMap[near] = oldNear
		} else {
			break
		}
	}
	mchan <- oldNear
}

// constrainNear will do a IK gradient descent from seedInputs to target. If a gradient descent distance
// function has been specified, this will use that.
// This function will return either a valid configuration that meets constraints, or nil.
func (mp *cBiRRTMotionPlanner) constrainNear(
	ctx context.Context,
	randseed *rand.Rand,
	seedInputs,
	target referenceframe.FrameSystemInputs,
) referenceframe.FrameSystemInputs {
	for i := 0; i < maxNearIter; i++ {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		newArc := &motionplan.SegmentFS{
			StartConfiguration: seedInputs,
			EndConfiguration:   target,
			FS:                 mp.fs,
		}

		// Check if the arc of "seedInputs" to "target" is valid
		ok, _ := mp.CheckSegmentAndStateValidityFS(newArc, mp.planOpts.Resolution)
		if ok {
			return target
		}
		solutionGen := make(chan *ik.Solution, 1)
		linearSeed, err := mp.lfs.mapToSlice(target)
		if err != nil {
			return nil
		}

		// Spawn the IK solver to generate solutions until done
		err = mp.fastGradDescent.Solve(ctx, solutionGen, linearSeed, mp.linearizeFSmetric(mp.ConstraintHandler.pathMetric), randseed.Int())
		// We should have zero or one solutions
		var solved *ik.Solution
		select {
		case solved = <-solutionGen:
		default:
		}
		close(solutionGen)
		if err != nil || solved == nil {
			return nil
		}
		solutionMap, err := mp.lfs.sliceToMap(solved.Configuration)
		if err != nil {
			return nil
		}

		ok, failpos := mp.CheckSegmentAndStateValidityFS(
			&motionplan.SegmentFS{
				StartConfiguration: seedInputs,
				EndConfiguration:   solutionMap,
				FS:                 mp.fs,
			},
			mp.planOpts.Resolution,
		)
		if ok {
			return solutionMap
		}
		if failpos != nil {
			dist := mp.configurationDistanceFunc(&motionplan.SegmentFS{
				StartConfiguration: target,
				EndConfiguration:   failpos.EndConfiguration,
			})
			if dist > mp.planOpts.InputIdentDist {
				// If we have a first failing position, and that target is updating (no infinite loop), then recurse
				seedInputs = failpos.StartConfiguration
				target = failpos.EndConfiguration
			}
		} else {
			return nil
		}
	}
	return nil
}

// smoothPath will pick two points at random along the path and attempt to do a fast gradient descent directly between
// them, which will cut off randomly-chosen points with odd joint angles into something that is a more intuitive motion.
func (mp *cBiRRTMotionPlanner) smoothPath(ctx context.Context, inputSteps []node) []node {
	toIter := int(math.Min(float64(len(inputSteps)*len(inputSteps)), float64(mp.planOpts.SmoothIter)))

	schan := make(chan node, 1)
	defer close(schan)

	for numCornersToPass := 2; numCornersToPass > 0; numCornersToPass-- {
		for iter := 0; iter < toIter/2 && len(inputSteps) > 3; iter++ {
			select {
			case <-ctx.Done():
				return inputSteps
			default:
			}
			// get start node of first edge. Cannot be either the last or second-to-last node.
			// Intn will return an int in the half-open interval [0,n)
			i := mp.randseed.Intn(len(inputSteps) - 2)
			j := i + 1
			cornersPassed := 0
			hitCorners := []node{}
			for (cornersPassed != numCornersToPass || !inputSteps[j].Corner()) && j < len(inputSteps)-1 {
				j++
				if cornersPassed < numCornersToPass && inputSteps[j].Corner() {
					cornersPassed++
					hitCorners = append(hitCorners, inputSteps[j])
				}
			}
			// no corners existed between i and end of inputSteps -> not good candidate for smoothing
			if len(hitCorners) == 0 {
				continue
			}

			shortcutGoal := make(map[node]node)

			iSol := inputSteps[i]
			jSol := inputSteps[j]
			shortcutGoal[jSol] = nil

			mp.constrainedExtend(ctx, mp.randseed, shortcutGoal, jSol, iSol, schan)
			reached := <-schan

			// Note this could technically replace paths with "longer" paths i.e. with more waypoints.
			// However, smoothed paths are invariably more intuitive and smooth, and lend themselves to future shortening,
			// so we allow elongation here.
			dist := mp.configurationDistanceFunc(&motionplan.SegmentFS{
				StartConfiguration: inputSteps[i].Q(),
				EndConfiguration:   reached.Q(),
			})
			if dist < mp.planOpts.InputIdentDist {
				for _, hitCorner := range hitCorners {
					hitCorner.SetCorner(false)
				}

				newInputSteps := append([]node{}, inputSteps[:i]...)
				for reached != nil {
					newInputSteps = append(newInputSteps, reached)
					reached = shortcutGoal[reached]
				}
				newInputSteps[i].SetCorner(true)
				newInputSteps[len(newInputSteps)-1].SetCorner(true)
				newInputSteps = append(newInputSteps, inputSteps[j+1:]...)
				inputSteps = newInputSteps
			}
		}
	}
	return inputSteps
}

// getFrameSteps will return a slice of positive values representing the largest amount a particular DOF of a frame should
// move in any given step. The second argument is a float describing the percentage of the total movement.
func getFrameSteps(lfs *linearizedFrameSystem, percentTotalMovement float64) map[string][]float64 {
	frameQstep := map[string][]float64{}
	for _, f := range lfs.frames {
		dof := f.DoF()
		pos := make([]float64, len(dof))
		for i, lim := range dof {
			l, u := lim.Min, lim.Max

			// Default to [-999,999] as range if limits are infinite
			if l == math.Inf(-1) {
				l = -999
			}
			if u == math.Inf(1) {
				u = 999
			}

			jRange := math.Abs(u - l)
			pos[i] = jRange * percentTotalMovement
		}
		frameQstep[f.Name()] = pos
	}
	return frameQstep
}
