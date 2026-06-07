package version

import "fmt"

var (
	// Version is overridden by release builds with -ldflags.
	Version = "dev"
	// Commit is overridden by release builds with -ldflags.
	Commit = "unknown"
	// Date is overridden by release builds with -ldflags.
	Date = "unknown"
)

type Info struct {
	Version string
	Commit  string
	Date    string
}

func Current() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
	}
}

func (i Info) String() string {
	if i.Commit == "" || i.Commit == "unknown" {
		return i.Version
	}

	if i.Date == "" || i.Date == "unknown" {
		return fmt.Sprintf("%s (%s)", i.Version, i.Commit)
	}

	return fmt.Sprintf("%s (%s, %s)", i.Version, i.Commit, i.Date)
}
