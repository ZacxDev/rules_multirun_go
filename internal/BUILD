load("@bazel_skylib//:bzl_library.bzl", "bzl_library")
load("@rules_go//go:def.bzl", "go_binary", "go_library")

go_binary(
    name = "multirun",
    embed = [":multirun_lib"],
    visibility = ["//visibility:public"],
)

go_library(
    name = "multirun_lib",
    srcs = ["multirun.go"],
    importpath = "github.com/ZacxDev/multirun",
    visibility = ["//visibility:private"],
    deps = [
      "@io_bazel_rules_go//go/runfiles",
    ],
)

bzl_library(
    name = "constants",
    srcs = ["constants.bzl"],
    visibility = ["//:__pkg__"],
)
