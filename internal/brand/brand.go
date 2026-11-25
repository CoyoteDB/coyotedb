// Package brand holds the product name in one place. "CoyoteDB" / "coyote" is
// a working name; renaming the product should only touch this file, the Go
// module path, and the marketing site.
package brand

const (
	// Product is the human-facing product name.
	Product = "CoyoteDB"
	// Command is the name of the program users type.
	Command = "coyote"
	// DirName is the folder name used for default data locations on
	// Windows and macOS (capitalised, as is usual there).
	DirName = "CoyoteDB"
	// DirNameLower is the folder name used on Linux.
	DirNameLower = "coyote"
	// EnvPrefix prefixes environment variables the program reads.
	EnvPrefix = "COYOTE_"
)

// DockerImage is the image the Docker join commands name (docker/README.md).
// A release image sets it to its own version at build time
// (-X coyotedb.com/coyote/internal/brand.DockerImage=coyotedb/coyote:0.1.0,
// docker/Dockerfile COYOTE_IMAGE), so every computer joins with the same
// version; other builds name edge.
var DockerImage = "coyotedb/coyote:edge"
