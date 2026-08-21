//go:build js || plan9 || wasip1 || windows

package normalizer

import "os"

func openSourceFile(root *os.Root, path string) (*os.File, error) {
	return root.Open(path)
}
