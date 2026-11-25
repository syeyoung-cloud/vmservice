package metadata

import (
	"encoding/binary"
	"unsafe"
)

func unsafePointer(b *byte) unsafe.Pointer { return unsafe.Pointer(b) }

// nativeUint32 reads a netlink field, which is in the host's byte order (little endian on amd64).
func nativeUint32(b []byte) uint32 { return binary.NativeEndian.Uint32(b) }
