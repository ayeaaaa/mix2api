package logs

import (
	"fmt"
	"io"
)

// WriteExport writes result as v1's plain-text log download:
//
//	[ts] [level] [tag] msg
//
// Rows are re-redacted at export time because a database written before the
// redaction rules existed may still contain plaintext values.
func WriteExport(writer io.Writer, result Result) error {
	if writer == nil {
		return fmt.Errorf("logs: nil export writer")
	}
	for index, entry := range result.Logs {
		if index > 0 {
			if _, err := io.WriteString(writer, "\n"); err != nil {
				return err
			}
		}
		message := RedactText(RedactQuery(entry.Msg))
		if _, err := fmt.Fprintf(writer, "[%s] [%s] [%s] %s", entry.TS, entry.Level, entry.Tag, message); err != nil {
			return err
		}
	}
	return nil
}
