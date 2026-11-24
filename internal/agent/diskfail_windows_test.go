package agent

// readOnlyErrno is the error isReadOnlyErrno looks for.
func readOnlyErrno() error { return errorWriteProtect }
