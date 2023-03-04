// Copyright 2019 The ebml-go authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webm

import (
	"github.com/at-wat/ebml-go/mkvcore"
)

// BlockWriter is a WebM block writer interface.
type BlockWriter interface {
	mkvcore.BlockWriter
}

// BlockReader is a WebM block reader interface.
type BlockReader interface {
	mkvcore.BlockReader
}

// BlockCloser is a WebM closer interface.
type BlockCloser interface {
	mkvcore.BlockCloser
}

// BlockWriteCloser groups Writer and Closer.
type BlockWriteCloser interface {
	mkvcore.BlockWriteCloser
}

// BlockReadCloser groups Reader and Closer.
type BlockReadCloser interface {
	mkvcore.BlockReadCloser
}

// FrameWriter is a backward compatibility wrapper of BlockWriteCloser.
//
// Deprecated: This is exposed to keep compatibility with the old version.
// Use BlockWriteCloser interface instead.
type FrameWriter struct {
	mkvcore.BlockWriteCloser
}
