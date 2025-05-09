//go:build debug
// +build debug

package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"

	"github.com/restic/restic/internal/crypto"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/repository/index"
	"github.com/restic/restic/internal/repository/pack"
	"github.com/restic/restic/internal/restic"
)

func registerDebugCommand(cmd *cobra.Command) {
	cmd.AddCommand(
		newDebugCommand(),
	)
}

func newDebugCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "debug",
		Short:             "Debug commands",
		GroupID:           cmdGroupDefault,
		DisableAutoGenTag: true,
	}
	cmd.AddCommand(newDebugDumpCommand())
	cmd.AddCommand(newDebugExamineCommand())
	return cmd
}

func newDebugDumpCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dump [indexes|snapshots|all|packs]",
		Short: "Dump data structures",
		Long: `
The "dump" command dumps data structures from the repository as JSON objects. It
is used for debugging purposes only.

EXIT STATUS
===========

Exit status is 0 if the command was successful.
Exit status is 1 if there was any error.
Exit status is 10 if the repository does not exist.
Exit status is 11 if the repository is already locked.
Exit status is 12 if the password is incorrect.
`,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugDump(cmd.Context(), globalOptions, args)
		},
	}
	return cmd
}

func newDebugExamineCommand() *cobra.Command {
	var opts DebugExamineOptions

	cmd := &cobra.Command{
		Use:               "examine pack-ID...",
		Short:             "Examine a pack file",
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugExamine(cmd.Context(), globalOptions, opts, args)
		},
	}

	opts.AddFlags(cmd.Flags())
	return cmd
}

type DebugExamineOptions struct {
	TryRepair     bool
	RepairByte    bool
	ExtractPack   bool
	ReuploadBlobs bool
}

func (opts *DebugExamineOptions) AddFlags(f *pflag.FlagSet) {
	f.BoolVar(&opts.ExtractPack, "extract-pack", false, "write blobs to the current directory")
	f.BoolVar(&opts.ReuploadBlobs, "reupload-blobs", false, "reupload blobs to the repository")
	f.BoolVar(&opts.TryRepair, "try-repair", false, "try to repair broken blobs with single bit flips")
	f.BoolVar(&opts.RepairByte, "repair-byte", false, "try to repair broken blobs by trying bytes")
}

func prettyPrintJSON(wr io.Writer, item interface{}) error {
	buf, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return err
	}

	_, err = wr.Write(append(buf, '\n'))
	return err
}

func debugPrintSnapshots(ctx context.Context, repo *repository.Repository, wr io.Writer) error {
	return restic.ForAllSnapshots(ctx, repo, repo, nil, func(id restic.ID, snapshot *restic.Snapshot, err error) error {
		if err != nil {
			return err
		}

		if _, err := fmt.Fprintf(wr, "snapshot_id: %v\n", id); err != nil {
			return err
		}

		return prettyPrintJSON(wr, snapshot)
	})
}

// Pack is the struct used in printPacks.
type Pack struct {
	Name string `json:"name"`

	Blobs []Blob `json:"blobs"`
}

// Blob is the struct used in printPacks.
type Blob struct {
	Type   restic.BlobType `json:"type"`
	Length uint            `json:"length"`
	ID     restic.ID       `json:"id"`
	Offset uint            `json:"offset"`
}

func printPacks(ctx context.Context, repo *repository.Repository, wr io.Writer) error {

	var m sync.Mutex
	return restic.ParallelList(ctx, repo, restic.PackFile, repo.Connections(), func(ctx context.Context, id restic.ID, size int64) error {
		blobs, _, err := repo.ListPack(ctx, id, size)
		if err != nil {
			Warnf("error for pack %v: %v\n", id.Str(), err)
			return nil
		}

		p := Pack{
			Name:  id.String(),
			Blobs: make([]Blob, len(blobs)),
		}
		for i, blob := range blobs {
			p.Blobs[i] = Blob{
				Type:   blob.Type,
				Length: blob.Length,
				ID:     blob.ID,
				Offset: blob.Offset,
			}
		}

		m.Lock()
		defer m.Unlock()
		return prettyPrintJSON(wr, p)
	})
}

func dumpIndexes(ctx context.Context, repo restic.ListerLoaderUnpacked, wr io.Writer) error {
	return index.ForAllIndexes(ctx, repo, repo, func(id restic.ID, idx *index.Index, err error) error {
		Printf("index_id: %v\n", id)
		if err != nil {
			return err
		}

		return idx.Dump(wr)
	})
}

func runDebugDump(ctx context.Context, gopts GlobalOptions, args []string) error {
	if len(args) != 1 {
		return errors.Fatal("type not specified")
	}

	ctx, repo, unlock, err := openWithReadLock(ctx, gopts, gopts.NoLock)
	if err != nil {
		return err
	}
	defer unlock()

	tpe := args[0]

	switch tpe {
	case "indexes":
		return dumpIndexes(ctx, repo, globalOptions.stdout)
	case "snapshots":
		return debugPrintSnapshots(ctx, repo, globalOptions.stdout)
	case "packs":
		return printPacks(ctx, repo, globalOptions.stdout)
	case "all":
		Printf("snapshots:\n")
		err := debugPrintSnapshots(ctx, repo, globalOptions.stdout)
		if err != nil {
			return err
		}

		Printf("\nindexes:\n")
		err = dumpIndexes(ctx, repo, globalOptions.stdout)
		if err != nil {
			return err
		}

		return nil
	default:
		return errors.Fatalf("no such type %q", tpe)
	}
}

func tryRepairWithBitflip(key *crypto.Key, input []byte, bytewise bool) []byte {
	if bytewise {
		Printf("        trying to repair blob by finding a broken byte\n")
	} else {
		Printf("        trying to repair blob with single bit flip\n")
	}

	ch := make(chan int)
	var wg errgroup.Group
	done := make(chan struct{})
	var fixed []byte
	var found bool

	workers := runtime.GOMAXPROCS(0)
	Printf("         spinning up %d worker functions\n", runtime.GOMAXPROCS(0))
	for i := 0; i < workers; i++ {
		wg.Go(func() error {
			// make a local copy of the buffer
			buf := make([]byte, len(input))
			copy(buf, input)

			testFlip := func(idx int, pattern byte) bool {
				// flip bits
				buf[idx] ^= pattern

				nonce, plaintext := buf[:key.NonceSize()], buf[key.NonceSize():]
				plaintext, err := key.Open(plaintext[:0], nonce, plaintext, nil)
				if err == nil {
					Printf("\n")
					Printf("        blob could be repaired by XORing byte %v with 0x%02x\n", idx, pattern)
					Printf("        hash is %v\n", restic.Hash(plaintext))
					close(done)
					found = true
					fixed = plaintext
					return true
				}

				// flip bits back
				buf[idx] ^= pattern
				return false
			}

			for i := range ch {
				if bytewise {
					for j := 0; j < 255; j++ {
						if testFlip(i, byte(j)) {
							return nil
						}
					}
				} else {
					for j := 0; j < 7; j++ {
						// flip each bit once
						if testFlip(i, (1 << uint(j))) {
							return nil
						}
					}
				}
			}
			return nil
		})
	}

	wg.Go(func() error {
		defer close(ch)

		start := time.Now()
		info := time.Now()
		for i := range input {
			select {
			case ch <- i:
			case <-done:
				Printf("     done after %v\n", time.Since(start))
				return nil
			}

			if time.Since(info) > time.Second {
				secs := time.Since(start).Seconds()
				gps := float64(i) / secs
				remaining := len(input) - i
				eta := time.Duration(float64(remaining)/gps) * time.Second

				Printf("\r%d byte of %d done (%.2f%%), %.0f byte per second, ETA %v",
					i, len(input), float32(i)/float32(len(input))*100, gps, eta)
				info = time.Now()
			}
		}
		return nil
	})
	err := wg.Wait()
	if err != nil {
		panic("all go routines can only return nil")
	}

	if !found {
		Printf("\n        blob could not be repaired\n")
	}
	return fixed
}

func decryptUnsigned(k *crypto.Key, buf []byte) []byte {
	// strip signature at the end
	l := len(buf)
	nonce, ct := buf[:16], buf[16:l-16]
	out := make([]byte, len(ct))

	c, err := aes.NewCipher(k.EncryptionKey[:])
	if err != nil {
		panic(fmt.Sprintf("unable to create cipher: %v", err))
	}
	e := cipher.NewCTR(c, nonce)
	e.XORKeyStream(out, ct)

	return out
}

func loadBlobs(ctx context.Context, opts DebugExamineOptions, repo restic.Repository, packID restic.ID, list []restic.Blob) error {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		panic(err)
	}

	pack, err := repo.LoadRaw(ctx, restic.PackFile, packID)
	// allow processing broken pack files
	if pack == nil {
		return err
	}

	wg, ctx := errgroup.WithContext(ctx)

	if opts.ReuploadBlobs {
		repo.StartPackUploader(ctx, wg)
	}

	wg.Go(func() error {
		for _, blob := range list {
			Printf("      loading blob %v at %v (length %v)\n", blob.ID, blob.Offset, blob.Length)
			if int(blob.Offset+blob.Length) > len(pack) {
				Warnf("skipping truncated blob\n")
				continue
			}
			buf := pack[blob.Offset : blob.Offset+blob.Length]
			key := repo.Key()

			nonce, plaintext := buf[:key.NonceSize()], buf[key.NonceSize():]
			plaintext, err = key.Open(plaintext[:0], nonce, plaintext, nil)
			outputPrefix := ""
			filePrefix := ""
			if err != nil {
				Warnf("error decrypting blob: %v\n", err)
				if opts.TryRepair || opts.RepairByte {
					plaintext = tryRepairWithBitflip(key, buf, opts.RepairByte)
				}
				if plaintext != nil {
					outputPrefix = "repaired "
					filePrefix = "repaired-"
				} else {
					plaintext = decryptUnsigned(key, buf)
					err = storePlainBlob(blob.ID, "damaged-", plaintext)
					if err != nil {
						return err
					}
					continue
				}
			}

			if blob.IsCompressed() {
				decompressed, err := dec.DecodeAll(plaintext, nil)
				if err != nil {
					Printf("         failed to decompress blob %v\n", blob.ID)
				}
				if decompressed != nil {
					plaintext = decompressed
				}
			}

			id := restic.Hash(plaintext)
			var prefix string
			if !id.Equal(blob.ID) {
				Printf("         successfully %vdecrypted blob (length %v), hash is %v, ID does not match, wanted %v\n", outputPrefix, len(plaintext), id, blob.ID)
				prefix = "wrong-hash-"
			} else {
				Printf("         successfully %vdecrypted blob (length %v), hash is %v, ID matches\n", outputPrefix, len(plaintext), id)
				prefix = "correct-"
			}
			if opts.ExtractPack {
				err = storePlainBlob(id, filePrefix+prefix, plaintext)
				if err != nil {
					return err
				}
			}
			if opts.ReuploadBlobs {
				_, _, _, err := repo.SaveBlob(ctx, blob.Type, plaintext, id, true)
				if err != nil {
					return err
				}
				Printf("         uploaded %v %v\n", blob.Type, id)
			}
		}

		if opts.ReuploadBlobs {
			return repo.Flush(ctx)
		}
		return nil
	})

	return wg.Wait()
}

func storePlainBlob(id restic.ID, prefix string, plain []byte) error {
	filename := fmt.Sprintf("%s%s.bin", prefix, id)
	f, err := os.Create(filename)
	if err != nil {
		return err
	}

	_, err = f.Write(plain)
	if err != nil {
		_ = f.Close()
		return err
	}

	err = f.Close()
	if err != nil {
		return err
	}

	Printf("decrypt of blob %v stored at %v\n", id, filename)
	return nil
}

func runDebugExamine(ctx context.Context, gopts GlobalOptions, opts DebugExamineOptions, args []string) error {
	if opts.ExtractPack && gopts.NoLock {
		return fmt.Errorf("--extract-pack and --no-lock are mutually exclusive")
	}

	ctx, repo, unlock, err := openWithAppendLock(ctx, gopts, gopts.NoLock)
	if err != nil {
		return err
	}
	defer unlock()

	ids := make([]restic.ID, 0)
	for _, name := range args {
		id, err := restic.ParseID(name)
		if err != nil {
			id, err = restic.Find(ctx, repo, restic.PackFile, name)
			if err != nil {
				Warnf("error: %v\n", err)
				continue
			}
		}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		return errors.Fatal("no pack files to examine")
	}

	bar := newIndexProgress(gopts.Quiet, gopts.JSON)
	err = repo.LoadIndex(ctx, bar)
	if err != nil {
		return err
	}

	for _, id := range ids {
		err := examinePack(ctx, opts, repo, id)
		if err != nil {
			Warnf("error: %v\n", err)
		}
		if err == context.Canceled {
			break
		}
	}
	return nil
}

func examinePack(ctx context.Context, opts DebugExamineOptions, repo restic.Repository, id restic.ID) error {
	Printf("examine %v\n", id)

	buf, err := repo.LoadRaw(ctx, restic.PackFile, id)
	// also process damaged pack files
	if buf == nil {
		return err
	}
	Printf("  file size is %v\n", len(buf))
	gotID := restic.Hash(buf)
	if !id.Equal(gotID) {
		Printf("  wanted hash %v, got %v\n", id, gotID)
	} else {
		Printf("  hash for file content matches\n")
	}

	Printf("  ========================================\n")
	Printf("  looking for info in the indexes\n")

	blobsLoaded := false
	// examine all data the indexes have for the pack file
	for b := range repo.ListPacksFromIndex(ctx, restic.NewIDSet(id)) {
		blobs := b.Blobs
		if len(blobs) == 0 {
			continue
		}

		checkPackSize(blobs, len(buf))

		err = loadBlobs(ctx, opts, repo, id, blobs)
		if err != nil {
			Warnf("error: %v\n", err)
		} else {
			blobsLoaded = true
		}
	}

	Printf("  ========================================\n")
	Printf("  inspect the pack itself\n")

	blobs, _, err := repo.ListPack(ctx, id, int64(len(buf)))
	if err != nil {
		return fmt.Errorf("pack %v: %v", id.Str(), err)
	}
	checkPackSize(blobs, len(buf))

	if !blobsLoaded {
		return loadBlobs(ctx, opts, repo, id, blobs)
	}
	return nil
}

func checkPackSize(blobs []restic.Blob, fileSize int) {
	// track current size and offset
	var size, offset uint64

	sort.Slice(blobs, func(i, j int) bool {
		return blobs[i].Offset < blobs[j].Offset
	})

	for _, pb := range blobs {
		Printf("      %v blob %v, offset %-6d, raw length %-6d\n", pb.Type, pb.ID, pb.Offset, pb.Length)
		if offset != uint64(pb.Offset) {
			Printf("      hole in file, want offset %v, got %v\n", offset, pb.Offset)
		}
		offset = uint64(pb.Offset + pb.Length)
		size += uint64(pb.Length)
	}
	size += uint64(pack.CalculateHeaderSize(blobs))

	if uint64(fileSize) != size {
		Printf("      file sizes do not match: computed %v, file size is %v\n", size, fileSize)
	} else {
		Printf("      file sizes match\n")
	}
}
