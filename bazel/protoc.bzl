"""protoc as the proto toolchain resolves it, for a genrule's tools."""

def _resolved_protoc_impl(ctx):
    compiler = ctx.toolchains["@protobuf//bazel/private:proto_toolchain_type"].proto.proto_compiler
    protoc = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(output = protoc, target_file = compiler.executable, is_executable = True)
    return [DefaultInfo(
        executable = protoc,
        files = depset([protoc]),
        runfiles = ctx.runfiles(files = [compiler.executable]),
    )]

resolved_protoc = rule(
    implementation = _resolved_protoc_impl,
    doc = """The protoc of the resolved proto toolchain, as an executable.

`@protobuf//:protoc` is always protoc built from source with the C++
toolchain. This is the protoc proto_library and go_proto_library run: the
prebuilt binary under `--@protobuf//bazel/toolchains:prefer_prebuilt_protoc`
(.bazelrc). Use it in a genrule's `tools`, built for the exec platform.""",
    executable = True,
    toolchains = ["@protobuf//bazel/private:proto_toolchain_type"],
)
