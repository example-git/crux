package providerplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

func (m *Manager) InspectImageSource(ctx context.Context, source string) (RegisteredImageBundle, error) {
	if err := ctx.Err(); err != nil {
		return RegisteredImageBundle{}, err
	}
	if source == "" || strings.Contains(source, "://") || !filepath.IsAbs(source) {
		return RegisteredImageBundle{}, errors.New("image setup requires an absolute local bundle directory")
	}
	temporary := filepath.Join(m.paths.Cache, ".image-preview-"+uuid.NewString())
	defer os.RemoveAll(temporary)
	snapshot, err := snapshotDirectory(source, temporary)
	if err != nil {
		return RegisteredImageBundle{}, errors.New("cannot snapshot image setup source")
	}
	report, bundle := diagnoseSnapshot(temporary, snapshot)
	if !report.Valid {
		return RegisteredImageBundle{}, &DiagnosticError{Report: report}
	}
	if bundle.image == nil {
		return RegisteredImageBundle{}, errors.New("image setup source is not an image-provider bundle")
	}
	return RegisteredImageBundle{Manifest: *bundle.image, Digest: snapshot.Digest}, nil
}
