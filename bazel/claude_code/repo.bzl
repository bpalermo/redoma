"""Claude Code's native binary, from its npm packages, pinned by version and sha256 per architecture.

The packages @anthropic-ai/claude-code-linux-x64 and -linux-arm64 hold the
executable at package/claude; npm's postinstall (install.cjs) copies it over a
placeholder, and this does the same with no Node. The file is exported as it
is in the tarball: Anthropic's terms require it unmodified, so nothing here
patches or strips it.

The repository has a directory per architecture, x64/ and arm64/, each with
the tarball's claude and package.json; :claude, the binary of the target
platform's architecture; and version.bzl, whose CLAUDE_CODE_VERSION is the
version pinned, for the tests and labels.
"""

_URL = "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-{arch}/-/claude-code-linux-{arch}-{version}.tgz"

# npm's architecture name: the platforms constraint it is the binary of.
_ARCHES = {
    "arm64": "@platforms//cpu:aarch64",
    "x64": "@platforms//cpu:x86_64",
}

def _claude_code_impl(rctx):
    if sorted(rctx.attr.sha256.keys()) != sorted(_ARCHES.keys()):
        fail("claude_code: sha256 must have exactly the keys %s" % sorted(_ARCHES.keys()))
    for arch in _ARCHES:
        rctx.download_and_extract(
            url = _URL.format(arch = arch, version = rctx.attr.version),
            sha256 = rctx.attr.sha256[arch],
            stripPrefix = "package",
            output = arch,
        )
        rctx.file(arch + "/BUILD.bazel", """\
exports_files(
    [
        "claude",
        "package.json",
    ],
    visibility = ["//visibility:public"],
)
""")
    rctx.file("version.bzl", 'CLAUDE_CODE_VERSION = "{}"\n'.format(rctx.attr.version))
    rctx.file("BUILD.bazel", """\
exports_files(["version.bzl"], visibility = ["//visibility:public"])

alias(
    name = "claude",
    actual = select({{
{selects}
    }}),
    visibility = ["//visibility:public"],
)
""".format(selects = "\n".join([
        '        "{}": "//{}:claude",'.format(constraint, arch)
        for arch, constraint in _ARCHES.items()
    ])))

claude_code = repository_rule(
    implementation = _claude_code_impl,
    doc = "Claude Code's linux-x64 and linux-arm64 binaries, from the npm registry.",
    attrs = {
        "version": attr.string(mandatory = True, doc = "The Claude Code version."),
        "sha256": attr.string_dict(mandatory = True, doc = "Each tarball's sha256, by npm architecture: x64 and arm64."),
    },
)
