package store

import "errors"

// errPreflightRollback is returned from inside a transaction to force a rollback after
// a successful dry run; PreflightApplyAssetEvent converts it back to nil.
var errPreflightRollback = errors.New("preflight dry run: rolled back")
