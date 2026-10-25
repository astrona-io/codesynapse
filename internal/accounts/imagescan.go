package accounts

import (
	"time"

	"github.com/astrona-io/codesynapse/internal/imagescan"
)

// ImageScan is the last container image report uploaded for a repository
// namespace ("org|graph", store.RuntimeGraph): the findings of its images
// from CI, replaced as a whole by the next upload. It lives here because a
// re-index of the code graph must not drop it and it belongs to the tenant.
type ImageScan struct {
	UploadedAt time.Time           `json:"uploaded_at"`
	Findings   []imagescan.Finding `json:"findings"`
}
