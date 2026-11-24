package main

// chownLike is not needed on Windows: the file inherits the data folder's
// ACL, which grants the service's virtual account full control.
func chownLike(path, ref string) error { return nil }
