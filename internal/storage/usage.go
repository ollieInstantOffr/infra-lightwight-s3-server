package storage

import (
	"fmt"
	"syscall"
)

// Usage describes the space situation on the volume backing the store. The
// console dashboard shows it, and it is the honest answer to "how much room is
// left", since a single-node store has nowhere else to put anything.
type Usage struct {
	// BytesUsed is the size of the blobs directory as tracked by the database.
	// Free and Total come from the filesystem.
	TotalBytes uint64
	FreeBytes  uint64
}

// UsedBytes is the space consumed on the volume, which includes anything else
// sharing it — not just this store.
func (u Usage) UsedBytes() uint64 {
	if u.TotalBytes < u.FreeBytes {
		return 0
	}
	return u.TotalBytes - u.FreeBytes
}

// Usage reports total and available space on the volume holding the store.
func (s *Store) Usage() (Usage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(s.root, &stat); err != nil {
		return Usage{}, fmt.Errorf("statfs %q: %w", s.root, err)
	}
	// Field widths differ between platforms, so everything is widened to uint64
	// before arithmetic.
	blockSize := uint64(stat.Bsize)
	return Usage{
		TotalBytes: uint64(stat.Blocks) * blockSize,
		// Bavail, not Bfree: blocks reserved for root are not usable by us.
		FreeBytes: uint64(stat.Bavail) * blockSize,
	}, nil
}
