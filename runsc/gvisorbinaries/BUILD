load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_test(
    name = "gvisorbinaries_test",
    size = "small",
    srcs = ["resolver_test.go"],
    library = ":gvisorbinaries",
    deps = [
        "//runsc/config",
        "//runsc/version",
    ],
)

go_library(
    name = "gvisorbinaries",
    srcs = ["gvisorbinaries.go"],
    visibility = [
        "//visibility:public",
    ],
    deps = [
        "//pkg/log",
        "//pkg/sync",
        "//runsc/config",
        "//runsc/specutils",
        "//runsc/version",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
