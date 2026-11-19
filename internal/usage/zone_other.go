//go:build !unix

package usage

// zoneName 在其他平台上還沒有做（Windows 以後再做，D25），退回 time.Local。
func zoneName() string { return "" }
