"""Files built for another platform than the build's own."""

def _platform_transition_impl(_settings, attr):
    return {"//command_line_option:platforms": str(attr.platform)}

_platform_transition = transition(
    implementation = _platform_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _platform_files_impl(ctx):
    return [DefaultInfo(files = depset(ctx.files.srcs))]

platform_files = rule(
    implementation = _platform_files_impl,
    doc = """The files of srcs, built for platform.

A pure-Go binary built this way is a cross-build: linux/arm64 on an amd64
host, for example. The rule passes on the files only, not the runfiles.""",
    attrs = {
        "platform": attr.label(mandatory = True, doc = "The platform srcs are built for."),
        "srcs": attr.label_list(allow_files = True, cfg = _platform_transition, doc = "What to build."),
    },
)
