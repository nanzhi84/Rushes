//go:build !e2e_scaffold

package main

import "github.com/nanzhi84/Rushes/go/internal/understanding"

func defaultAnalyzer() *understanding.Analyzer {
	return understanding.NewAnalyzer(nil)
}
