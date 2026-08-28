package rpc

import "errors"

// asError exists only so rpc.go reads without an errors.As inline. Split out
// because the generic form needs a typed target and that is noisier at the call
// site than the intent deserves.
func asError(err error, target **Error) bool { return errors.As(err, target) }
