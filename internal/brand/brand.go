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
	// DockerImage is the published image people run (docker/README.md).
	DockerImage = "coyotedb/coyote:edge"
)
