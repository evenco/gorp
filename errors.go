package gorp

import (
	"github.com/evenco/go-errors"
)

var (
	ErrorMismatchedSchema       = errors.Define("The database returned columns that don't exist in the model type.")
	ErrorConcurrentModification = errors.Define("The model was updated by another agent.")
)

// returns true if the error is non-fatal (ie, we shouldn't immediately return)
func NonFatalError(err error) bool {
	return errors.Is(err, ErrorMismatchedSchema)
}
