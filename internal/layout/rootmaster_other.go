//go:build !unix

package layout

import "os"

func ownedByRoot(os.FileInfo) bool { return false }
