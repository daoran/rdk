//go:build !windows

package motionplan

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"github.com/edaniels/golog"
	"github.com/golang/geo/r3"
	"go.viam.com/test"

	"go.viam.com/rdk/motionplan/ik"
	"go.viam.com/rdk/motionplan/tpspace"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

var printPath = false

const testTurnRad = 0.3

func TestPtgRrt(t *testing.T) {
	t.Parallel()
	logger := golog.NewTestLogger(t)
	roverGeom, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{10, 10, 10}, "")
	test.That(t, err, test.ShouldBeNil)
	geometries := []spatialmath.Geometry{roverGeom}

	ctx := context.Background()

	ackermanFrame, err := tpspace.NewPTGFrameFromTurningRadius(
		"ackframe",
		logger,
		300.,
		testTurnRad,
		0,
		geometries,
	)
	test.That(t, err, test.ShouldBeNil)

	goalPos := spatialmath.NewPose(r3.Vector{X: 200, Y: 7000, Z: 0}, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 90})

	opt := newBasicPlannerOptions(ackermanFrame)
	opt.DistanceFunc = ik.NewSquaredNormSegmentMetric(30.)
	mp, err := newTPSpaceMotionPlanner(ackermanFrame, rand.New(rand.NewSource(42)), logger, opt)
	test.That(t, err, test.ShouldBeNil)
	tp, ok := mp.(*tpSpaceRRTMotionPlanner)
	tp.algOpts.pathdebug = printPath
	if tp.algOpts.pathdebug {
		tp.logger.Debug("$type,X,Y")
		tp.logger.Debugf("$SG,%f,%f\n", 0., 0.)
		tp.logger.Debugf("$SG,%f,%f\n", goalPos.Point().X, goalPos.Point().Y)
	}
	test.That(t, ok, test.ShouldBeTrue)
	plan, err := tp.plan(ctx, goalPos, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(plan), test.ShouldBeGreaterThanOrEqualTo, 2)

	allPtgs := ackermanFrame.(tpspace.PTGProvider).PTGs()
	lastPose := spatialmath.NewZeroPose()

	if tp.algOpts.pathdebug {
		for _, mynode := range plan {
			trajPts, _ := allPtgs[int(mynode.Q()[0].Value)].Trajectory(mynode.Q()[1].Value, mynode.Q()[2].Value)
			for i, pt := range trajPts {
				intPose := spatialmath.Compose(lastPose, pt.Pose)
				if i == 0 {
					tp.logger.Debugf("$WP,%f,%f\n", intPose.Point().X, intPose.Point().Y)
				}
				tp.logger.Debugf("$FINALPATH,%f,%f\n", intPose.Point().X, intPose.Point().Y)
				if i == len(trajPts)-1 {
					lastPose = intPose
					break
				}
			}
		}
	}
	plan = tp.smoothPath(ctx, plan)
	if tp.algOpts.pathdebug {
		lastPose = spatialmath.NewZeroPose()
		for _, mynode := range plan {
			trajPts, _ := allPtgs[int(mynode.Q()[0].Value)].Trajectory(mynode.Q()[1].Value, mynode.Q()[2].Value)
			for i, pt := range trajPts {
				intPose := spatialmath.Compose(lastPose, pt.Pose)
				if i == 0 {
					tp.logger.Debugf("$SMOOTHWP,%f,%f\n", intPose.Point().X, intPose.Point().Y)
				}
				tp.logger.Debugf("$SMOOTHPATH,%f,%f\n", intPose.Point().X, intPose.Point().Y)
				if pt.Dist >= mynode.Q()[2].Value {
					lastPose = intPose
					break
				}
			}
		}
	}
}

func TestPtgWithObstacle(t *testing.T) {
	t.Parallel()
	logger := golog.NewTestLogger(t)
	roverGeom, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{10, 10, 10}, "")
	test.That(t, err, test.ShouldBeNil)
	geometries := []spatialmath.Geometry{roverGeom}
	ackermanFrame, err := tpspace.NewPTGFrameFromTurningRadius(
		"ackframe",
		logger,
		300.,
		testTurnRad,
		0,
		geometries,
	)
	test.That(t, err, test.ShouldBeNil)

	ctx := context.Background()

	goalPos := spatialmath.NewPoseFromPoint(r3.Vector{X: 6500, Y: 0, Z: 0})

	fs := referenceframe.NewEmptyFrameSystem("test")
	fs.AddFrame(ackermanFrame, fs.World())

	opt := newBasicPlannerOptions(ackermanFrame)
	opt.DistanceFunc = ik.NewSquaredNormSegmentMetric(30.)
	opt.GoalThreshold = 5
	// obstacles
	obstacle1, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{3300, -500, 0}), r3.Vector{180, 1800, 1}, "")
	test.That(t, err, test.ShouldBeNil)
	obstacle2, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{3300, 1800, 0}), r3.Vector{180, 1800, 1}, "")
	test.That(t, err, test.ShouldBeNil)
	obstacle3, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{2500, -1400, 0}), r3.Vector{50000, 30, 1}, "")
	test.That(t, err, test.ShouldBeNil)
	obstacle4, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{2500, 2400, 0}), r3.Vector{50000, 30, 1}, "")
	test.That(t, err, test.ShouldBeNil)

	geoms := []spatialmath.Geometry{obstacle1, obstacle2, obstacle3, obstacle4}

	worldState, err := referenceframe.NewWorldState(
		[]*referenceframe.GeometriesInFrame{referenceframe.NewGeometriesInFrame(referenceframe.World, geoms)},
		nil,
	)
	test.That(t, err, test.ShouldBeNil)
	sf, err := newSolverFrame(fs, ackermanFrame.Name(), referenceframe.World, nil)
	test.That(t, err, test.ShouldBeNil)
	collisionConstraints, err := createAllCollisionConstraints(sf, fs, worldState, referenceframe.StartPositions(fs), nil)
	test.That(t, err, test.ShouldBeNil)

	for name, constraint := range collisionConstraints {
		opt.AddStateConstraint(name, constraint)
	}

	mp, err := newTPSpaceMotionPlanner(ackermanFrame, rand.New(rand.NewSource(42)), logger, opt)
	test.That(t, err, test.ShouldBeNil)
	tp, _ := mp.(*tpSpaceRRTMotionPlanner)
	tp.algOpts.pathdebug = printPath
	if tp.algOpts.pathdebug {
		tp.logger.Debug("$type,X,Y")
		for _, geom := range geoms {
			pts := geom.ToPoints(1.)
			for _, pt := range pts {
				if math.Abs(pt.Z) < 0.1 {
					tp.logger.Debugf("$OBS,%f,%f\n", pt.X, pt.Y)
				}
			}
		}
		tp.logger.Debugf("$SG,%f,%f\n", 0., 0.)
		tp.logger.Debugf("$SG,%f,%f\n", goalPos.Point().X, goalPos.Point().Y)
	}
	plan, err := tp.plan(ctx, goalPos, nil)

	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(plan), test.ShouldBeGreaterThan, 2)

	allPtgs := ackermanFrame.(tpspace.PTGProvider).PTGs()
	lastPose := spatialmath.NewZeroPose()

	if tp.algOpts.pathdebug {
		for _, mynode := range plan {
			trajPts, _ := allPtgs[int(mynode.Q()[0].Value)].Trajectory(mynode.Q()[1].Value, mynode.Q()[2].Value)
			for i, pt := range trajPts {
				intPose := spatialmath.Compose(lastPose, pt.Pose)
				if i == 0 {
					tp.logger.Debugf("$WP,%f,%f\n", intPose.Point().X, intPose.Point().Y)
				}
				tp.logger.Debugf("$FINALPATH,%f,%f\n", intPose.Point().X, intPose.Point().Y)
				if i == len(trajPts)-1 {
					lastPose = intPose
					break
				}
			}
		}
	}
	plan = tp.smoothPath(ctx, plan)
	if tp.algOpts.pathdebug {
		lastPose = spatialmath.NewZeroPose()
		for _, mynode := range plan {
			trajPts, _ := allPtgs[int(mynode.Q()[0].Value)].Trajectory(mynode.Q()[1].Value, mynode.Q()[2].Value)
			for i, pt := range trajPts {
				intPose := spatialmath.Compose(lastPose, pt.Pose)
				if i == 0 {
					tp.logger.Debugf("$SMOOTHWP,%f,%f\n", intPose.Point().X, intPose.Point().Y)
				}
				tp.logger.Debugf("$SMOOTHPATH,%f,%f\n", intPose.Point().X, intPose.Point().Y)
				if pt.Dist >= mynode.Q()[2].Value {
					lastPose = intPose
					break
				}
			}
		}
	}
}

func TestIKPtgRrt(t *testing.T) {
	t.Parallel()
	logger := golog.NewTestLogger(t)
	roverGeom, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{10, 10, 10}, "")
	test.That(t, err, test.ShouldBeNil)
	geometries := []spatialmath.Geometry{roverGeom}

	ackermanFrame, err := tpspace.NewPTGFrameFromTurningRadius(
		"ackframe",
		logger,
		300.,
		testTurnRad,
		0,
		geometries,
	)
	test.That(t, err, test.ShouldBeNil)

	goalPos := spatialmath.NewPose(r3.Vector{X: 50, Y: 10, Z: 0}, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 180})

	opt := newBasicPlannerOptions(ackermanFrame)
	opt.SetGoalMetric(ik.NewPositionOnlyMetric(goalPos))
	opt.DistanceFunc = ik.SquaredNormNoOrientSegmentMetric
	opt.GoalThreshold = 10.
	mp, err := newTPSpaceMotionPlanner(ackermanFrame, rand.New(rand.NewSource(42)), logger, opt)
	test.That(t, err, test.ShouldBeNil)
	tp, ok := mp.(*tpSpaceRRTMotionPlanner)
	test.That(t, ok, test.ShouldBeTrue)

	plan, err := tp.plan(context.Background(), goalPos, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(plan), test.ShouldBeGreaterThanOrEqualTo, 2)
}

func TestTPsmoothing(t *testing.T) {
	// TODO: this doesn't smooth properly yet. This should be made to smooth better.
	t.Parallel()
	logger := golog.NewTestLogger(t)
	roverGeom, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{10, 10, 10}, "")
	test.That(t, err, test.ShouldBeNil)
	geometries := []spatialmath.Geometry{roverGeom}

	ctx := context.Background()

	ackermanFrame, err := tpspace.NewPTGFrameFromTurningRadius(
		"ackframe",
		logger,
		300.,
		testTurnRad,
		0,
		geometries,
	)
	test.That(t, err, test.ShouldBeNil)

	opt := newBasicPlannerOptions(ackermanFrame)
	opt.DistanceFunc = ik.NewSquaredNormSegmentMetric(30.)
	mp, err := newTPSpaceMotionPlanner(ackermanFrame, rand.New(rand.NewSource(42)), logger, opt)
	test.That(t, err, test.ShouldBeNil)
	tp, _ := mp.(*tpSpaceRRTMotionPlanner)

	// plan which is known to be able to use some smoothing
	planInputs := [][]referenceframe.Input{
		{{0}, {0}, {0}},
		{{3}, {-0.20713797715976653}, {848.2300164692441}},
		{{5}, {0.0314906475636095}, {848.2300108402619}},
		{{5}, {0.0016660735709435135}, {848.2300146893297}},
		{{0}, {0.00021343061342569985}, {408}},
		{{5}, {1.9088870836327245}, {737.7547597081078}},
		{{2}, {-1.3118738553451883}, {848.2300164692441}},
		{{0}, {-3.1070696573964987}, {848.2300164692441}},
		{{0}, {-2.5547017183037877}, {306}},
		{{4}, {-2.31209484211255}, {408}},
		{{0}, {1.1943809502464207}, {571.4368241014894}},
		{{0}, {0.724950779684863}, {848.2300164692441}},
		{{0}, {-1.2295409308605127}, {848.2294213788913}},
		{{5}, {2.677652944060827}, {848.230013198154}},
		{{0}, {2.7618396954635545}, {848.2300164692441}},
		{{0}, {0}, {0}},
	}
	plan := []node{}
	for _, inp := range planInputs {
		thisNode := &basicNode{
			q:    inp,
			cost: inp[2].Value,
		}
		plan = append(plan, thisNode)
	}
	plan, err = rectifyTPspacePath(plan, tp.frame)
	test.That(t, err, test.ShouldBeNil)

	plan = tp.smoothPath(ctx, plan)
	test.That(t, plan, test.ShouldNotBeNil)
}
