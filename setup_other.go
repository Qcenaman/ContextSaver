//go:build !windows

package main

// The Windows ContextSaver's setup steps; elsewhere only the GitHub sign-in applies.
func setupSteps() []setupStep { return []setupStep{githubStep()} }
