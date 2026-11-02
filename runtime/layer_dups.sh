# shellcheck shell=bash
# layer_dups reads a tar listing (tar -t) on stdin and prints each path it
# names more than once, one per line, sorted. Paths are normalized first: no
# leading ./ and no trailing /, so ./etc/ and etc are the same path; the
# archive's root (. or ./) is not a path.
#
# A listing with no path at all, an empty layer's, prints nothing and
# succeeds. grep's 1 (no line selected) is that case; any other failure of
# the pipeline, grep's 2 included, is the function's: the body is a subshell
# with its own pipefail, so that holds whatever the caller set.
layer_dups() (
	set -o pipefail
	sed -e 's#^\./##' -e 's#/$##' |
		{ grep -v '^\.\?$' || (($? == 1)); } |
		sort |
		uniq -d
)
