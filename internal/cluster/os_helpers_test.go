package cluster

import "os"

func writeFileOS(path string, b []byte) error { return os.WriteFile(path, b, 0o644) }
func readFileOS(path string) ([]byte, error)  { return os.ReadFile(path) }
