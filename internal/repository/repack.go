package repository

import (
	"context"
	"crypto/sha256"
	"io"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/hashing"
	"github.com/restic/restic/internal/pack"
	"github.com/restic/restic/internal/restic"

	"github.com/restic/restic/internal/errors"
)

// Repack takes a list of packs together with a list of blobs contained in
// these packs. Each pack is loaded and the blobs listed in keepBlobs is saved
// into a new pack. Returned is the list of obsolete packs which can then
// be removed.
func Repack(ctx context.Context, repo restic.Repository, packs restic.IDSet, keepBlobs restic.BlobSet, p *restic.Progress) (obsoletePacks restic.IDSet, err error) {
	debug.Log("repacking %d packs while keeping %d blobs", len(packs), len(keepBlobs))

	for packID := range packs {
		// load the complete pack into a temp file
		h := restic.Handle{Type: restic.DataFile, Name: packID.String()}

		tempfile, err := fs.TempFile("", "restic-temp-repack-")
		if err != nil {
			return nil, errors.Wrap(err, "TempFile")
		}

		beRd, err := repo.Backend().Load(ctx, h, 0, 0)
		if err != nil {
			return nil, err
		}

		hrd := hashing.NewReader(beRd, sha256.New())
		packLength, err := io.Copy(tempfile, hrd)
		if err != nil {
			return nil, errors.Wrap(err, "Copy")
		}

		if err = beRd.Close(); err != nil {
			return nil, errors.Wrap(err, "Close")
		}

		hash := restic.IDFromHash(hrd.Sum(nil))
		debug.Log("pack %v loaded (%d bytes), hash %v", packID.Str(), packLength, hash.Str())

		if !packID.Equal(hash) {
			return nil, errors.Errorf("hash does not match id: want %v, got %v", packID, hash)
		}

		_, err = tempfile.Seek(0, 0)
		if err != nil {
			return nil, errors.Wrap(err, "Seek")
		}

		blobs, err := pack.List(repo.Key(), tempfile, packLength)
		if err != nil {
			return nil, err
		}

		debug.Log("processing pack %v, blobs: %v", packID.Str(), len(blobs))
		var buf []byte
		for _, entry := range blobs {
			h := restic.BlobHandle{ID: entry.ID, Type: entry.Type}
			if !keepBlobs.Has(h) {
				continue
			}

			debug.Log("  process blob %v", h)

			buf = buf[:]
			if uint(len(buf)) < entry.Length {
				buf = make([]byte, entry.Length)
			}
			buf = buf[:entry.Length]

			n, err := tempfile.ReadAt(buf, int64(entry.Offset))
			if err != nil {
				return nil, errors.Wrap(err, "ReadAt")
			}

			if n != len(buf) {
				return nil, errors.Errorf("read blob %v from %v: not enough bytes read, want %v, got %v",
					h, tempfile.Name(), len(buf), n)
			}

			n, err = repo.Key().Decrypt(buf, buf)
			if err != nil {
				return nil, err
			}

			buf = buf[:n]

			id := restic.Hash(buf)
			if !id.Equal(entry.ID) {
				return nil, errors.Errorf("read blob %v from %v: wrong data returned, hash is %v",
					h, tempfile.Name(), id)
			}

			_, err = repo.SaveBlob(ctx, entry.Type, buf, entry.ID)
			if err != nil {
				return nil, err
			}

			debug.Log("  saved blob %v", entry.ID.Str())

			keepBlobs.Delete(h)
		}

		if err = tempfile.Close(); err != nil {
			return nil, errors.Wrap(err, "Close")
		}

		if err = fs.RemoveIfExists(tempfile.Name()); err != nil {
			return nil, errors.Wrap(err, "Remove")
		}
		if p != nil {
			p.Report(restic.Stat{Blobs: 1})
		}
	}

	if err := repo.Flush(); err != nil {
		return nil, err
	}

	return packs, nil
}
