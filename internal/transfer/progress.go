package transfer

import (
	"github.com/Findddx/pku-drive-cli/internal/download"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

// advancedOnlyDownload forwards only per-file byte deltas to the aggregate
// progress; Started/Finished are owned by the tree coordinator. Mirrors the
// sharelink advancedOnlyProgress pattern; a separate type exists because
// download.Progress and upload.Progress are distinct interface types.
type advancedOnlyDownload struct{ parent download.Progress }

func (p advancedOnlyDownload) Started(int64) {}

func (p advancedOnlyDownload) Advanced(delta int64) {
	if p.parent != nil {
		p.parent.Advanced(delta)
	}
}

func (p advancedOnlyDownload) Finished() {}

// advancedOnlyUpload is the upload-side counterpart of advancedOnlyDownload.
type advancedOnlyUpload struct{ parent upload.Progress }

func (p advancedOnlyUpload) Started(int64) {}

func (p advancedOnlyUpload) Advanced(delta int64) {
	if p.parent != nil {
		p.parent.Advanced(delta)
	}
}

func (p advancedOnlyUpload) Finished() {}
