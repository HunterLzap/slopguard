package main

import "context"

type AutostartManager interface {
	Enabled(context.Context) (bool, error)
	SetEnabled(context.Context, bool) error
}
