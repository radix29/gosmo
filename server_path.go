package gosmo

import "strings"

// Paths on the server's own file system. A path belongs to the server, not
// the client: gosmo runs on Linux against Windows instances routinely, so
// path/filepath (the client's rules) would treat a Windows path as one name.
// These split on either separator and join with the one the path already
// uses.

// JoinServerPath appends file to dir, a directory on the server's file
// system, with the separator dir already uses (a backslash if it has one,
// "/" otherwise). A dir already ending in a separator is kept as it is, so a
// root (`C:\`, "/", `\\host\share\`) joins exactly; an empty dir — no
// directory known — leaves file bare, for the server to place. An empty file
// yields dir with a trailing separator.
func JoinServerPath(dir, file string) string {
	if dir == "" {
		return file
	}
	if strings.HasSuffix(dir, `\`) || strings.HasSuffix(dir, "/") {
		return dir + file
	}
	return dir + serverPathSeparator(dir) + file
}

// ServerPathBase returns the file-name part of a server path — everything
// after its last separator, either one — or the whole path if it has none.
func ServerPathBase(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ServerPathDir returns the directory part of a server path, without a
// trailing separator except at a root: `C:\x\a.bak` is `C:\x`, `C:\a.bak`
// is `C:\`, "/a.bak" is "/". A bare name has no directory (""). Joining the
// result with ServerPathBase gives the path back, unless it mixes separators.
func ServerPathDir(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	if i < 0 {
		return ""
	}
	dir := path[:i]
	if dir == "" || (len(dir) == 2 && dir[1] == ':') {
		return path[:i+1]
	}
	return dir
}

// ServerPathExt returns the extension (".mdf") of a server path's file name,
// or "" if it has none. A leading dot is not an extension: ".ldf" has none.
func ServerPathExt(path string) string {
	base := ServerPathBase(path)
	if i := strings.LastIndex(base, "."); i > 0 {
		return base[i:]
	}
	return ""
}

// serverPathSeparator returns the separator a server-side path uses — a
// backslash for Windows paths, "/" otherwise. Deciding from the path itself
// rather than from runtime.GOOS is what keeps a Linux client correct against
// a Windows server and vice versa.
func serverPathSeparator(path string) string {
	if strings.Contains(path, `\`) {
		return `\`
	}
	return "/"
}
