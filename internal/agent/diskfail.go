package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"coyotedb.com/coyote/internal/cluster"
)

// A disk that stops taking writes (turned read-only, full, or failing)
// under this node's cluster records stops the node: Raft cannot go on
// without saving its term (R1, cluster.ErrStorage). storageMessage says
// so in plain words, naming the folder.

// storageMessage is the stop message for a cluster.ErrStorage error.
// clusterDir is named when the error does not carry a path.
func storageMessage(err error, clusterDir string) string {
	dir, cause := clusterDir, strings.TrimPrefix(err.Error(), cluster.ErrStorage.Msg+": ")
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Path != "" {
		dir, cause = filepath.Dir(pe.Path), pe.Err.Error()
	}
	const stopped = "This node stopped to keep the data safe; the other nodes take over if there are enough of them. "
	switch {
	case readOnlyFS(err):
		return fmt.Sprintf("This computer's disk stopped accepting writes (%s: read-only file system). "+stopped+
			"Fix the disk, then start this node again.", dir)
	case diskFull(err):
		return fmt.Sprintf("The disk that holds %s is full, so this node could not save its cluster records. "+stopped+
			"Free some space on it, then start this node again.", dir)
	}
	return fmt.Sprintf("This computer's disk refused a write in %s (%s). "+stopped+
		"Check the disk, then start this node again.", dir, cause)
}

// readOnlyFS: the file system refuses all writes (mounted or turned
// read-only, write-protected media).
func readOnlyFS(err error) bool {
	if isReadOnlyErrno(err) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "read-only file system") || strings.Contains(s, "media is write protected")
}
