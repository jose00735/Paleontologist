//go:build !windows

package main

import "errors"

func mapDriveLetter(want string, port int, label string) (string, error) {
	return "", errors.New("drive letters are only available on Windows")
}
func unmapDriveLetter(letter string) {}
func openExplorer(path string)       {}
func fixWebDAVLimit() error          { return errors.New("this command is for Windows") }
