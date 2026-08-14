package debuginfod

import "io"

// Metadata carries the debuginfod response headers that accompany an artifact.
// Fields are zero when the server omits the corresponding header.
type Metadata struct {
	// Size is the artifact size in bytes from X-DEBUGINFOD-SIZE.
	// It may differ from the HTTP Content-Length if the body is compressed in transit.
	Size int64 `json:"size"`
	// File is the suggested file name from X-DEBUGINFOD-FILE, e.g. "/usr/lib/debug/.../ls.debug".
	File string `json:"file"`
	// Archive is the source archive name from X-DEBUGINFOD-ARCHIVE, set when the artifact was extracted from one.
	Archive string `json:"archive"`
	// IMASignature is the per-file IMA signature from X-DEBUGINFOD-IMASIGNATURE, decoded from its hex wire form.
	IMASignature []byte `json:"ima"`
}

// Response is the result of a successful fetch: the artifact body plus its metadata.
// The embedded io.ReadCloser lets callers stream the body directly via Read and Close.
type Response struct {
	io.ReadCloser
	Meta Metadata
}
