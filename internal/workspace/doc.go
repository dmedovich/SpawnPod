// Package workspace owns bounded tar extraction and archiving under one job
// workspace. It also creates canonical Runner working directories before
// execution. All path operations use os.Root to contain concurrent symlink
// changes within the mounted workspace.
package workspace
