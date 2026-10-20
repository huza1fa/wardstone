//go:build !linux

package research

import (
	"context"
	"errors"
	"os"
)

func runPDFCommand(context.Context, []byte, string, ...string) ([]byte, string) {
	return nil, "pdf_extractor_unavailable"
}
func openLocal(string) (*os.File, error) {
	return nil, errors.New("configured local files require Linux confined file access")
}
