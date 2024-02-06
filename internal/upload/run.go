// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package upload

import (
	"io"
	"log"
	"runtime"
	"time"

	"golang.org/x/telemetry"
	it "golang.org/x/telemetry/internal/telemetry"
)

var logger *log.Logger

func init() {
	logger = log.New(io.Discard, "", 0)
}

// SetLogOutput sets the default logger's output destination.
func SetLogOutput(logging io.Writer) {
	if logging != nil {
		logger.SetOutput(logging)
	}
}

// Uploader carries parameters needed for upload.
type Uploader struct {
	// Config is used to select counters to upload.
	Config *telemetry.UploadConfig
	// ConfigVersion is the version of the config.
	ConfigVersion string

	// LocalDir is where the local counter files are.
	LocalDir string
	// UploadDir is where uploader leaves the copy of uploaded data.
	UploadDir string
	// ModeFilePath is the file.
	ModeFilePath it.ModeFilePath

	UploadServerURL string
	StartTime       time.Time

	cache parsedCache
}

// NewUploader creates a default uploader.
func NewUploader(config *telemetry.UploadConfig) *Uploader {
	return &Uploader{
		Config:          config,
		ConfigVersion:   "custom",
		LocalDir:        it.LocalDir,
		UploadDir:       it.UploadDir,
		ModeFilePath:    it.ModeFile,
		UploadServerURL: "https://telemetry.go.dev/upload",
		StartTime:       time.Now().UTC(),
	}
}

// disabledOnPlatform indicates whether telemetry is disabled
// due to bugs in the current platform.
const disabledOnPlatform = false ||
	// The following platforms could potentially be supported in the future:
	runtime.GOOS == "openbsd" || // #60614
	runtime.GOOS == "solaris" || // #60968 #60970
	runtime.GOOS == "android" || // #60967
	// These platforms fundamentally can't be supported:
	runtime.GOOS == "js" || // #60971
	runtime.GOOS == "wasip1" || // #60971
	// Work is in progress to support 386:
	runtime.GOARCH == "386" // #60615 #60692 #60965 #60967

// Run generates and uploads reports
func (u *Uploader) Run() {
	if disabledOnPlatform {
		return
	}
	todo := u.findWork()
	ready, err := u.reports(&todo)
	if err != nil {
		logger.Printf("reports: %v", err)
	}
	for _, f := range ready {
		u.uploadReport(f)
	}
}
