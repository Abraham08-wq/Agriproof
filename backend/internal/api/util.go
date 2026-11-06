package api

import "fmt"

func wrap(err error, msg string) error { return fmt.Errorf("%w: %s", err, msg) }
