// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Telemetrygodev serves the telemetry.go.dev website.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/exp/slog"
	"golang.org/x/mod/semver"
	"golang.org/x/telemetry/godev/internal/config"
	"golang.org/x/telemetry/godev/internal/content"
	ilog "golang.org/x/telemetry/godev/internal/log"
	"golang.org/x/telemetry/godev/internal/middleware"
	"golang.org/x/telemetry/godev/internal/storage"
	"golang.org/x/telemetry/internal/chartconfig"
	tconfig "golang.org/x/telemetry/internal/config"
	contentfs "golang.org/x/telemetry/internal/content"
	"golang.org/x/telemetry/internal/telemetry"
	"golang.org/x/telemetry/internal/unionfs"
)

func main() {
	flag.Parse()
	ctx := context.Background()
	cfg := config.NewConfig()

	if cfg.UseGCS {
		// We are likely running on GCP. Use GCP logging JSON format.
		slog.SetDefault(slog.New(ilog.NewGCPLogHandler()))
	}

	handler := newHandler(ctx, cfg)

	fmt.Printf("server listening at http://localhost:%s\n", cfg.ServerPort)
	log.Fatal(http.ListenAndServe(":"+cfg.ServerPort, handler))
}

func newHandler(ctx context.Context, cfg *config.Config) http.Handler {
	buckets, err := storage.NewAPI(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	ucfg, err := tconfig.ReadConfig(cfg.UploadConfig)
	if err != nil {
		log.Fatal(err)
	}
	fsys := fsys(cfg.DevMode)
	mux := http.NewServeMux()

	logger := slog.Default()
	mux.Handle("/", handleRoot(fsys, buckets))
	mux.Handle("/config", handleConfig(fsys, ucfg))
	mux.Handle("/upload/", handleUpload(ucfg, buckets, logger))
	mux.Handle("/charts/", handleChart(fsys, buckets))

	mw := middleware.Chain(
		middleware.Log(logger),
		middleware.Timeout(cfg.RequestTimeout),
		middleware.RequestSize(cfg.MaxRequestBytes),
		middleware.Recover(),
	)
	return mw(mux)
}

type link struct {
	Text, URL string
}

type indexPage struct {
	Charts  []*link
	Reports []*link
}

func handleRoot(fsys fs.FS, buckets *storage.API) content.HandlerFunc {
	cserv := content.Server(fsys)
	return func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path != "/" {
			cserv.ServeHTTP(w, r)
			return nil
		}
		page := indexPage{}

		ctx := r.Context()
		it := buckets.Chart.Objects(ctx, "")
		for {
			obj, err := it.Next()
			if errors.Is(err, storage.ErrObjectIteratorDone) {
				break
			}
			date := strings.TrimSuffix(obj, ".json")
			page.Charts = append(page.Charts, &link{Text: date, URL: "/charts/" + date})
		}
		it = buckets.Merge.Objects(ctx, "")
		for {
			obj, err := it.Next()
			if errors.Is(err, storage.ErrObjectIteratorDone) {
				break
			}
			page.Reports = append(page.Reports, &link{
				Text: strings.TrimSuffix(obj, ".json"),
				URL:  buckets.Merge.URI() + "/" + obj,
			})
		}
		return content.Template(w, fsys, "index.html", page, http.StatusOK)
	}
}

type chartPage struct {
	Charts map[string]any
}

func handleChart(fsys fs.FS, buckets *storage.API) content.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		ctx := r.Context()
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/charts/")
		reader, err := buckets.Chart.Object(p + ".json").NewReader(ctx)
		if errors.Is(err, storage.ErrObjectNotExist) {
			return content.Status(w, http.StatusNotFound)
		} else if err != nil {
			return err
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		var charts map[string]any
		if err := json.Unmarshal(data, &charts); err != nil {
			return err
		}
		page := chartPage{
			Charts: charts,
		}
		return content.Template(w, fsys, "charts.html", page, http.StatusOK)
	}
}

func handleUpload(ucfg *tconfig.Config, buckets *storage.API, log *slog.Logger) content.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if r.Method == "POST" {
			ctx := r.Context()
			// Log the error, but only the first 80 characters.
			// This prevents excessive logging related to broken payloads.
			// The first line should give us a sense of the failure mode.
			warn := func(msg string, err error) {
				errs := []rune(err.Error())
				if len(errs) > 80 {
					errs = append(errs[:79], '…')
				}
				log.WarnContext(ctx, msg+": "+string(errs))
			}
			var report telemetry.Report
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				warn("invalid JSON payload", err)
				return content.Error(err, http.StatusBadRequest)
			}
			if err := validate(&report, ucfg); err != nil {
				warn("invalid report", err)
				return content.Error(err, http.StatusBadRequest)
			}
			// TODO: capture metrics for collisions.
			name := fmt.Sprintf("%s/%g.json", report.Week, report.X)
			f, err := buckets.Upload.Object(name).NewWriter(ctx)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := json.NewEncoder(f).Encode(report); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			return content.Status(w, http.StatusOK)
		}
		return content.Status(w, http.StatusMethodNotAllowed)
	}
}

// validate validates the telemetry report data against the latest config.
func validate(r *telemetry.Report, cfg *tconfig.Config) error {
	// TODO: reject/drop data arrived too early or too late.
	if _, err := time.Parse("2006-01-02", r.Week); err != nil {
		return fmt.Errorf("invalid week %s", r.Week)
	}
	if !semver.IsValid(r.Config) {
		return fmt.Errorf("invalid config %s", r.Config)
	}
	if r.X == 0 {
		return fmt.Errorf("invalid X %g", r.X)
	}
	// TODO: We can probably keep known programs and counters even when a report
	// includes something that has been removed from the latest config.
	for _, p := range r.Programs {
		if !cfg.HasGOARCH(p.GOARCH) ||
			!cfg.HasGOOS(p.GOOS) ||
			!cfg.HasGoVersion(p.GoVersion) ||
			!cfg.HasProgram(p.Program) ||
			!cfg.HasVersion(p.Program, p.Version) {
			return fmt.Errorf("unknown program build %s@%q %q %s/%s", p.Program, p.Version, p.GoVersion, p.GOOS, p.GOARCH)
		}
		for c := range p.Counters {
			if !cfg.HasCounter(p.Program, c) {
				return fmt.Errorf("unknown counter %s", c)
			}
		}
		for s := range p.Stacks {
			prefix, _, _ := strings.Cut(s, "\n")
			if !cfg.HasStack(p.Program, prefix) {
				return fmt.Errorf("unknown stack %s", s)
			}
		}
	}
	return nil
}

func fsys(fromOS bool) fs.FS {
	var f fs.FS = contentfs.FS
	if fromOS {
		f = os.DirFS("internal/content")
		contentfs.RunESBuild(true)
	}
	f, err := unionfs.Sub(f, "telemetrygodev", "shared")
	if err != nil {
		log.Fatal(err)
	}
	return f
}

func handleConfig(fsys fs.FS, ucfg *tconfig.Config) content.HandlerFunc {
	ccfg := chartconfig.Raw()
	cfg := ucfg.UploadConfig
	version := "default"

	return func(w http.ResponseWriter, r *http.Request) error {
		cfgJSON, err := json.MarshalIndent(cfg, "", "\t")
		if err != nil {
			cfgJSON = []byte("unknown")
		}
		data := struct {
			Version      string
			ChartConfig  string
			UploadConfig string
		}{
			Version:      version,
			ChartConfig:  string(ccfg),
			UploadConfig: string(cfgJSON),
		}
		return content.Template(w, fsys, "config.html", data, http.StatusOK)
	}
}
