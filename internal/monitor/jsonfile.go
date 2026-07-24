package monitor

import (
	"encoding/json"
	"fmt"
	"os"
)

// readJSONFile decodes a JSON file into v. A missing file is reported as an
// error like any other; callers that treat absence as "nothing yet" check for
// it themselves.
func readJSONFile(name string, v any) error {
	raw, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

// writeJSONFile writes v to name as indented JSON, atomically. The files it
// writes are small state records a human may well open while debugging, so the
// extra bytes of indentation buy real legibility.
func writeJSONFile(name string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	return writeFileAtomic(name, append(raw, '\n'))
}
