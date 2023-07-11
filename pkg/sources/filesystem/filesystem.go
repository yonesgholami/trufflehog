package filesystem

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	diskbufferreader "github.com/bill-rich/disk-buffer-reader"
	"github.com/go-errors/errors"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/trufflesecurity/trufflehog/v3/pkg/common"
	"github.com/trufflesecurity/trufflehog/v3/pkg/context"
	"github.com/trufflesecurity/trufflehog/v3/pkg/handlers"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/source_metadatapb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/sourcespb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/sanitizer"
	"github.com/trufflesecurity/trufflehog/v3/pkg/sources"
)

const (
	// These buffer sizes are mainly driven by our largest credential size, which is GCP @ ~2.25KB.
	// Having a peek size larger than that ensures that we have complete credential coverage in our chunks.
	BufferSize = 10 * 1024 // 10KB
	PeekSize   = 3 * 1024  // 3KB
)

type Source struct {
	name     string
	sourceId int64
	jobId    int64
	verify   bool
	paths    []string
	log      logr.Logger
	filter   *common.Filter
	sources.Progress
	sources.CommonSourceUnitUnmarshaller
}

// Ensure the Source satisfies the interfaces at compile time
var _ sources.Source = (*Source)(nil)
var _ sources.SourceUnitUnmarshaller = (*Source)(nil)
var _ sources.SourceUnitEnumerator = (*Source)(nil)
var _ sources.SourceUnitChunker = (*Source)(nil)

// Type returns the type of source.
// It is used for matching source types in configuration and job input.
func (s *Source) Type() sourcespb.SourceType {
	return sourcespb.SourceType_SOURCE_TYPE_FILESYSTEM
}

func (s *Source) SourceID() int64 {
	return s.sourceId
}

func (s *Source) JobID() int64 {
	return s.jobId
}

// Init returns an initialized Filesystem source.
func (s *Source) Init(aCtx context.Context, name string, jobId, sourceId int64, verify bool, connection *anypb.Any, _ int) error {
	s.log = aCtx.Logger()

	s.name = name
	s.sourceId = sourceId
	s.jobId = jobId
	s.verify = verify

	var conn sourcespb.Filesystem
	if err := anypb.UnmarshalTo(connection, &conn, proto.UnmarshalOptions{}); err != nil {
		return errors.WrapPrefix(err, "error unmarshalling connection", 0)
	}
	s.paths = append(conn.Paths, conn.Directories...)

	return nil
}

func (s *Source) WithFilter(filter *common.Filter) {
	s.filter = filter
}

// Chunks emits chunks of bytes over a channel.
func (s *Source) Chunks(ctx context.Context, chunksChan chan *sources.Chunk) error {
	for i, path := range s.paths {
		logger := ctx.Logger().WithValues("path", path)
		if common.IsDone(ctx) {
			return nil
		}
		s.SetProgressComplete(i, len(s.paths), fmt.Sprintf("Path: %s", path), "")

		cleanPath := filepath.Clean(path)
		fileInfo, err := os.Stat(cleanPath)
		if err != nil {
			logger.Error(err, "unable to get file info")
			continue
		}

		if fileInfo.IsDir() {
			err = s.scanDir(ctx, cleanPath, chunksChan)
		} else {
			err = s.scanFile(ctx, cleanPath, chunksChan)
		}

		if err != nil && err != io.EOF {
			logger.Info("error scanning filesystem", "error", err)
		}
	}
	return nil
}

func (s *Source) scanDir(ctx context.Context, path string, chunksChan chan *sources.Chunk) error {
	return fs.WalkDir(os.DirFS(path), ".", func(relativePath string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fullPath := filepath.Join(path, relativePath)

		// Skip over non-regular files. We do this check here to suppress noisy
		// logs for trying to scan directories and other non-regular files in
		// our traversal.
		fileStat, err := os.Stat(fullPath)
		if err != nil {
			ctx.Logger().Info("unable to stat file", "path", fullPath, "error", err)
			return nil
		}
		if !fileStat.Mode().IsRegular() {
			return nil
		}
		if s.filter != nil && !s.filter.Pass(fullPath) {
			return nil
		}

		if err = s.scanFile(ctx, fullPath, chunksChan); err != nil {
			ctx.Logger().Info("error scanning file", "path", fullPath, "error", err)
		}
		return nil
	})
}

func (s *Source) scanFile(ctx context.Context, path string, chunksChan chan *sources.Chunk) error {
	logger := ctx.Logger().WithValues("path", path)
	fileStat, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("unable to stat file: %w", err)
	}
	if !fileStat.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}

	inputFile, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("unable to open file: %w", err)
	}
	defer inputFile.Close()
	logger.V(3).Info("scanning file")

	reReader, err := diskbufferreader.New(inputFile)
	if err != nil {
		return fmt.Errorf("could not create re-readable reader: %w", err)
	}
	defer reReader.Close()

	chunkSkel := &sources.Chunk{
		SourceType: s.Type(),
		SourceName: s.name,
		SourceID:   s.SourceID(),
		SourceMetadata: &source_metadatapb.MetaData{
			Data: &source_metadatapb.MetaData_Filesystem{
				Filesystem: &source_metadatapb.Filesystem{
					File: sanitizer.UTF8(path),
				},
			},
		},
		Verify: s.verify,
	}
	if handlers.HandleFile(ctx, reReader, chunkSkel, chunksChan) {
		return nil
	}

	if err := reReader.Reset(); err != nil {
		return err
	}
	reReader.Stop()

	for {
		chunkBytes := make([]byte, BufferSize)
		reader := bufio.NewReaderSize(reReader, BufferSize)
		n, err := reader.Read(chunkBytes)
		if err != nil && !errors.Is(err, io.EOF) {
			break
		}
		peekData, _ := reader.Peek(PeekSize)
		if n > 0 {
			chunksChan <- &sources.Chunk{
				SourceType: s.Type(),
				SourceName: s.name,
				SourceID:   s.SourceID(),
				Data:       append(chunkBytes[:n], peekData...),
				SourceMetadata: &source_metadatapb.MetaData{
					Data: &source_metadatapb.MetaData_Filesystem{
						Filesystem: &source_metadatapb.Filesystem{
							File: sanitizer.UTF8(path),
						},
					},
				},
				Verify: s.verify,
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return nil
}

// Enumerate implements SourceUnitEnumerator interface. This implementation simply
// passes the configured paths as the source unit, whether it be a single
// filepath or a directory.
func (s *Source) Enumerate(ctx context.Context, units chan<- sources.EnumerationResult) error {
	for _, path := range s.paths {
		item := sources.CommonEnumerationOk(path)
		if err := common.CancellableWrite(ctx, units, item); err != nil {
			return err
		}
	}
	return nil
}

// ChunkUnit implements SourceUnitChunker interface.
func (s *Source) ChunkUnit(ctx context.Context, unit sources.SourceUnit, chunks chan<- sources.ChunkResult) error {
	path := unit.SourceUnitID()
	logger := ctx.Logger().WithValues("path", path)

	cleanPath := filepath.Clean(path)
	fileInfo, err := os.Stat(cleanPath)
	if err != nil {
		return common.CancellableWrite(ctx, chunks, sources.ChunkErr(fmt.Errorf("unable to get file info: %w", err)))
	}

	ch := make(chan *sources.Chunk)
	go func() {
		defer close(ch)
		if fileInfo.IsDir() {
			// TODO: Finer grain error tracking of individual chunks.
			err = s.scanDir(ctx, cleanPath, ch)
		} else {
			// TODO: Finer grain error tracking of individual
			// chunks (in the case of archives).
			err = s.scanFile(ctx, cleanPath, ch)
		}
	}()

	for chunk := range ch {
		if chunk == nil {
			continue
		}
		if err := common.CancellableWrite(ctx, chunks, sources.ChunkOk(*chunk)); err != nil {
			return err
		}
	}

	if err != nil && err != io.EOF {
		logger.Info("error scanning filesystem", "error", err)
		return common.CancellableWrite(ctx, chunks, sources.ChunkErr(err))
	}
	return nil
}
