package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// DefaultTimeFormatStr is the default time format string for log appenders.
const DefaultTimeFormatStr = "2006-01-02T15:04:05.000Z0700"

// Appender is an output for log entries. This is a subset of the `zapcore.Core` interface.
type Appender interface {
	// Write submits a structured log entry to the appender for logging.
	Write(zapcore.Entry, []zapcore.Field) error
	// Sync is for signaling that any buffered logs to `Write` should be flushed. E.g: at shutdown.
	Sync() error
}

// ConsoleAppender will create human readable lines from log events and write them to the desired
// output sync. E.g: stdout or a file.
type ConsoleAppender struct {
	io.Writer
}

// NewStdoutAppender creates a new appender that prints to stdout.
func NewStdoutAppender() ConsoleAppender {
	return ConsoleAppender{os.Stdout}
}

// NewWriterAppender creates a new appender that prints to the input writer.
func NewWriterAppender(writer io.Writer) ConsoleAppender {
	return ConsoleAppender{writer}
}

// NewFileAppender will create an Appender that writes output to a log file. Log rotation will be
// enabled such that restarts of the viam-server with the same filename will move the old file out
// of the way. The `io.Closer` can be used to eventually close the opened log file.
func NewFileAppender(filename string) (Appender, io.Closer) {
	logger := &lumberjack.Logger{
		Filename: filename,
		// 1 Terabyte -- basically infinite. Don't rollover on size. Just restarts.
		MaxSize: 1024 * 1024,
	}

	// Dan: If we're restarting, explicitly call `Rotate` to write to a different file. This is a
	// convention I think is nice, but by no means a correctness requirement.
	if err := logger.Rotate(); err != nil {
		globalLogger.Error("Error creating log file:", err)
	}

	// We only have `NewFileAppender` return an io.Closer, rather than `NewWriterAppender` because
	// `NewWriterAppender` accepts stdout from `NewStdoutAppender`. And I'm not certain that it's a
	// good idea to be calling `stdout.Close`.
	return NewWriterAppender(logger), logger
}

// ZapcoreFieldsToJSON will serialize the Field objects into a JSON map of key/value pairs. It's
// unclear what circumstances will result in an error being returned.
func ZapcoreFieldsToJSON(fields []zapcore.Field) (result string, err error) {
	// Use zap's json encoder which will encode our slice of fields in-order. As opposed to the
	// random iteration order of a map. Call it with an empty Entry object such that only the fields
	// become "map-ified".
	// The json encoder can panic if there is a mismatch between the value in zapcore.Field.Type and
	// the data in the other fields, which happens in several cases as a result of proto serialization.
	// We attempt to sanitize incoming data in FieldFromProto, but recover here in case something slips
	// through to avoid crashing the entire goroutine.
	defer func() {
		if r := recover(); r != nil {
			if perr, ok := r.(error); ok {
				err = fmt.Errorf("panic serializing log fields: %w", perr)
				return
			}
			err = fmt.Errorf("panic serializing log fields: %v", r)
		}
	}()
	jsonEncoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{SkipLineEnding: true})
	buf, err := jsonEncoder.EncodeEntry(zapcore.Entry{}, fields)
	if err != nil {
		return "", err
	}

	return string(buf.Bytes()), nil
}

// Write outputs the log entry to the underlying stream.
func (appender ConsoleAppender) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	const maxLength = 10
	toPrint := make([]string, 0, maxLength)
	// We use UTC such that logs from different `viam-server`s can have their logs compared without
	// needing them to be configured in the same timezone.
	toPrint = append(toPrint, entry.Time.UTC().Format(DefaultTimeFormatStr))
	toPrint = append(toPrint, strings.ToUpper(entry.Level.String()))
	toPrint = append(toPrint, entry.LoggerName)
	if entry.Caller.Defined {
		toPrint = append(toPrint, callerToString(&entry.Caller))
	}
	toPrint = append(toPrint, entry.Message)
	if len(fields) == 0 {
		fmt.Fprintln(appender.Writer, strings.Join(toPrint, "\t")) //nolint:errcheck
		return nil
	}

	fieldsJSON, err := ZapcoreFieldsToJSON(fields)
	if err != nil {
		errJSON, err := json.Marshal(map[string]string{"logging_err": err.Error()})
		if err != nil {
			// This should never happen but append the raw sting as a last resort just in case.
			toPrint = append(toPrint, err.Error())
		} else {
			toPrint = append(toPrint, string(errJSON))
		}
	} else {
		toPrint = append(toPrint, fieldsJSON)
	}

	fmt.Fprintln(appender.Writer, strings.Join(toPrint, "\t")) //nolint:errcheck
	return nil
}

// Sync is a no-op.
func (appender ConsoleAppender) Sync() error {
	return nil
}

// The input `caller` must satisfy `caller.Defined == true`.
func callerToString(caller *zapcore.EntryCaller) string {
	// The file returned by `runtime.Caller` is a full path and always contains '/' to separate
	// directories. Including on windows. We only want to keep the `<package>/<file>` part of the
	// path. We use a stateful lambda to count back two '/' runes.
	cnt := 0
	idx := strings.LastIndexFunc(caller.File, func(rn rune) bool {
		if rn == '/' {
			cnt++
		}

		if cnt == 2 {
			return true
		}

		return false
	})

	// If idx >= 0, then we add 1 to trim the leading '/'.
	// If idx == -1 (not found), we add 1 to return the entire file.
	return fmt.Sprintf("%s:%d", caller.File[idx+1:], caller.Line)
}
