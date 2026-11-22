// tachyon-cache is the regional GitHub-compatible Actions cache service. It
// serves the Twirp cache API and signed blob URLs. Blobs live in an
// S3-compatible object store (-s3-endpoint, decision D4), on local disk
// (-dir, single-node development) or in memory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/decover-tech/tachyon/internal/cache"
)

func main() {
	addr := flag.String("http", ":8090", "listen address")
	publicURL := flag.String("public-url", "", "base URL VMs reach this service at, e.g. http://cache.r1.tachyon.internal:8090")
	dir := flag.String("dir", "", "store blobs under this directory (default: in memory)")
	s3Endpoint := flag.String("s3-endpoint", "", "S3-compatible object store URL, e.g. https://s3.r1.tachyon.internal:3900 (path-style addressing)")
	s3Bucket := flag.String("s3-bucket", "", "bucket for cache blobs (must already exist)")
	s3Region := flag.String("s3-region", "", "region the store signs for (Garage: its s3_region, often \"garage\"; MinIO: us-east-1)")
	s3Prefix := flag.String("s3-prefix", "", "optional key prefix inside the bucket, e.g. cache/")
	s3PartMB := flag.Int64("s3-part-mb", cache.DefaultS3PartSize>>20, "multipart part size in MiB (min 5); blobs under one part are a single PUT; memory use is about this per concurrent upload")
	s3KeyFile := flag.String("s3-secret-key-file", "", "file holding the secret key (alternative to TACHYON_S3_SECRET_KEY)")
	s3IDFile := flag.String("s3-access-key-id-file", "", "file holding the access key id (alternative to TACHYON_S3_ACCESS_KEY_ID)")
	quotaGB := flag.Int64("quota-gb", cache.DefaultQuotaBytes>>30, "per-repo cache quota in GB")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	key := os.Getenv("TACHYON_CACHE_KEY")
	if key == "" || *publicURL == "" {
		log.Error("need TACHYON_CACHE_KEY (shared with the control plane) and -public-url")
		os.Exit(2)
	}
	var store cache.BlobStore = &cache.MemStore{}
	switch {
	case *s3Endpoint != "" && *dir != "":
		log.Error("-s3-endpoint and -dir are mutually exclusive")
		os.Exit(2)
	case *s3Endpoint != "":
		creds, err := loadS3Credentials(*s3IDFile, *s3KeyFile)
		if err != nil {
			log.Error("s3 credentials", "err", err)
			os.Exit(2)
		}
		s3 := &cache.S3Store{Endpoint: *s3Endpoint, Bucket: *s3Bucket, Region: *s3Region,
			Prefix: *s3Prefix, Creds: creds, PartSize: *s3PartMB << 20}
		if err := s3.Validate(); err != nil {
			log.Error("s3 config", "err", err)
			os.Exit(2)
		}
		// Fail at start, not on the first cache restore, if the bucket or
		// credentials are wrong.
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = s3.Check(cctx)
		cancel()
		if err != nil {
			log.Error("s3 bucket check failed (does the bucket exist, and do the credentials have access?)",
				"endpoint", *s3Endpoint, "bucket", *s3Bucket, "err", err)
			os.Exit(1)
		}
		store = s3
		log.Info("blob store: s3", "endpoint", *s3Endpoint, "bucket", *s3Bucket, "region", *s3Region)
	case *dir != "":
		store = cache.DirStore{Root: *dir}
		log.Info("blob store: local directory (single node, development)", "dir", *dir)
	default:
		log.Warn("blob store: memory (contents are lost on restart)")
	}
	ix := &cache.Index{QuotaBytes: *quotaGB << 30}
	srv := cache.NewServer(ix, store, &cache.Signer{Key: []byte(key)}, *publicURL)
	srv.Log = log

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()
	log.Info("cache service up", "http", *addr, "public_url", *publicURL)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
}

// loadS3Credentials reads the key pair from the environment or from files.
// The secret is never accepted as a command-line flag, where ps would show it.
func loadS3Credentials(idFile, keyFile string) (cache.S3Credentials, error) {
	get := func(env, file string) (string, error) {
		if file != "" {
			b, err := os.ReadFile(file)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(b)), nil
		}
		return os.Getenv(env), nil
	}
	id, err := get("TACHYON_S3_ACCESS_KEY_ID", idFile)
	if err != nil {
		return cache.S3Credentials{}, err
	}
	secret, err := get("TACHYON_S3_SECRET_KEY", keyFile)
	if err != nil {
		return cache.S3Credentials{}, err
	}
	if id == "" || secret == "" {
		return cache.S3Credentials{}, fmt.Errorf("set TACHYON_S3_ACCESS_KEY_ID and TACHYON_S3_SECRET_KEY (or the -s3-*-file flags)")
	}
	return cache.S3Credentials{AccessKey: id, SecretKey: secret}, nil
}
