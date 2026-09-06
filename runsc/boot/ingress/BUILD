load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ingress",
    srcs = ["ingress.go"],
    visibility = ["//runsc:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "ingress_test",
    srcs = ["ingress_test.go"],
    library = ":ingress",
)
