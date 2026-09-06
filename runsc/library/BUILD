load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "library",
    srcs = [
        "compat.go",
        "container.go",
        "doc.go",
        "errors.go",
        "library.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/log",
        "//pkg/sentry/control",
        "//pkg/state/statefile",
        "//runsc/boot",
        "//runsc/compat",
        "//runsc/config",
        "//runsc/container",
        "//runsc/flag",
        "//runsc/sandbox",
        "//runsc/specutils",
        "@com_github_opencontainers_runtime_spec//specs-go:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "library_test",
    size = "large",
    srcs = [
        "ingress_test.go",
        "library_test.go",
    ],
    data = ["//:release"],
    library = ":library",
    deps = [
        "//pkg/log",
        "//pkg/sentry/control",
        "//pkg/state/statefile",
        "//pkg/test/testutil",
        "//runsc/boot/ingress",
        "//runsc/compat",
        "//runsc/config",
        "//runsc/flag",
        "//runsc/specutils",
        "@com_github_opencontainers_runtime_spec//specs-go:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
