package gorp

import (
	"github.com/evenco/go-errors"
)

var (
	ErrorConcurrentModification = errors.Define("The model was updated by another agent.")
)
