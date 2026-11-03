package webclient

import "github.com/example-git/crux/internal/attachmenthttp"

type attachmentBackend = attachmenthttp.Backend

var fileAttachment = attachmenthttp.Handler
