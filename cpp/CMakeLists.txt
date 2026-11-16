# The C++ port of Clepsydra (docs/specification/conformance.md: a port has
# its own build in its own top-level folder). cpp/README.md has the build,
# the tiers and the porting rules.
cmake_minimum_required(VERSION 3.20)
project(clepsydra VERSION 0.1.0 LANGUAGES CXX)

# Built on its own, or inside another project (add_subdirectory): there it
# builds the library alone and leaves the build type to that project.
# (PROJECT_IS_TOP_LEVEL needs CMake 3.21.)
if(CMAKE_SOURCE_DIR STREQUAL PROJECT_SOURCE_DIR)
  set(CLEPSYDRA_TOP_LEVEL ON)
else()
  set(CLEPSYDRA_TOP_LEVEL OFF)
endif()

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
if(CLEPSYDRA_TOP_LEVEL AND NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE RelWithDebInfo)
endif()

option(CLEPSYDRA_WERROR "Treat warnings as errors (the smoke tier's vet)" OFF)
option(CLEPSYDRA_TSAN "Build with ThreadSanitizer (the stress tier's race detector)" OFF)
option(CLEPSYDRA_TESTS "Build the test binary, the tools and the benchmarks" ${CLEPSYDRA_TOP_LEVEL})

# Link-time optimization in the optimized configurations, on request. Go
# compiles the reference's package as one unit and inlines across its files;
# here each Go file is a translation unit of its own, and only the linker can
# inline across them (cpp/README.md, "Speed"). Never under the sanitizers, and
# not where the toolchain has no LTO archiver. The library's archive then holds
# the compiler's intermediate code: link it with the same toolchain.
option(CLEPSYDRA_LTO "Link-time optimization where the toolchain supports it" OFF)
if(CLEPSYDRA_LTO AND NOT CLEPSYDRA_TSAN AND NOT CLEPSYDRA_SANITIZE)
  include(CheckIPOSupported)
  check_ipo_supported(RESULT clepsydra_lto LANGUAGES CXX)
  if(clepsydra_lto)
    foreach(config IN ITEMS RELEASE RELWITHDEBINFO MINSIZEREL)
      set(CMAKE_INTERPROCEDURAL_OPTIMIZATION_${config} ON)
    endforeach()
  else()
    message(STATUS "clepsydra: the toolchain can't link-time optimize (Clang: no llvm-ar found); building without")
  endif()
endif()

add_library(clepsydra
  src/api.cpp
  src/batch.cpp
  src/buzz.cpp
  src/ct.cpp
  src/cuckoo.cpp
  src/decay.cpp
  src/filter.cpp
  src/ghost.cpp
  src/hot.cpp
  src/merge.cpp
  src/overflow.cpp
  src/register.cpp
  src/resize.cpp
  src/slots.cpp
  src/snapshot.cpp
  src/stash.cpp
  src/touch.cpp
)
add_library(clepsydra::clepsydra ALIAS clepsydra)
# The public header needs C++20 (std::span, a defaulted operator==): every
# target that links the library builds as C++20 or later.
target_compile_features(clepsydra PUBLIC cxx_std_20)
target_include_directories(clepsydra PUBLIC include PRIVATE src)
find_package(Threads REQUIRED)
target_link_libraries(clepsydra PUBLIC Threads::Threads)

# Floating point must round like the reference's: no fused multiply-add
# contraction (Go on amd64 doesn't fuse) and no fast-math, or estimates can
# differ from spec/vectors in the last bit.
if(MSVC)
  # C4324 (a structure padded for an alignment specifier) is the point of
  # Core's alignas(64) fields and padded mutexes: one cache line each.
  target_compile_options(clepsydra PRIVATE /fp:precise /W4 /wd4324)
  if(CLEPSYDRA_WERROR)
    target_compile_options(clepsydra PRIVATE /WX)
  endif()
else()
  target_compile_options(clepsydra PRIVATE -ffp-contract=off -fno-fast-math -Wall -Wextra -Wpedantic)
  # Clang also checks what MSVC's /W4 does (a local hiding a member: C4458)
  # and what the C++ Core Guidelines and OpenSSF's hardening guide ask for,
  # all clean here. Sign conversions stay off: Go's index arithmetic mixes
  # int and uint64 at 112 sites; narrowing ones are none, and stay so.
  if(CMAKE_CXX_COMPILER_ID MATCHES "Clang")
    target_compile_options(clepsydra PRIVATE -Wshadow-all -Wconversion -Wno-sign-conversion -Wimplicit-fallthrough
                                             -Wformat=2 -Wold-style-cast -Wextra-semi -Wnon-virtual-dtor
                                             -Woverloaded-virtual -Wcast-align -Wnull-dereference -Wdouble-promotion)
  endif()
  if(CLEPSYDRA_WERROR)
    target_compile_options(clepsydra PRIVATE -Werror)
  endif()
endif()

if(CLEPSYDRA_TSAN)
  target_compile_options(clepsydra PUBLIC -fsanitize=thread -g)
  target_link_options(clepsydra PUBLIC -fsanitize=thread)
endif()

# Undefined behaviour and memory errors, which Go rules out and C++ doesn't:
# CLEPSYDRA_SANITIZE takes -fsanitize's list (address,undefined in CI), and
# turns on the standard library's bounds checks (libstdc++'s assertions,
# libc++'s extensive hardening; each library ignores the other's macro).
# CLEPSYDRA_SANITIZE_TRAP traps instead of reporting, for toolchains without
# the sanitizer runtimes (zig's Clang: -DCLEPSYDRA_SANITIZE=undefined).
set(CLEPSYDRA_SANITIZE "" CACHE STRING "Sanitizers to build with, as -fsanitize= takes them (e.g. address,undefined)")
option(CLEPSYDRA_SANITIZE_TRAP "Trap on a sanitizer's finding instead of reporting it (no runtime needed)" OFF)
if(CLEPSYDRA_SANITIZE AND NOT MSVC)
  set(clepsydra_san -fsanitize=${CLEPSYDRA_SANITIZE} -fno-sanitize-recover=all -fno-omit-frame-pointer -g)
  if(CLEPSYDRA_SANITIZE_TRAP)
    list(APPEND clepsydra_san -fsanitize-trap=all -fno-sanitize=vptr)
  endif()
  target_compile_options(clepsydra PUBLIC ${clepsydra_san})
  target_link_options(clepsydra PUBLIC ${clepsydra_san})
  target_compile_definitions(clepsydra PUBLIC _GLIBCXX_ASSERTIONS
                                              _LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_EXTENSIVE)
endif()

if(CLEPSYDRA_TESTS)
  # Every tests/*_test.cpp, each the port of the go/*_test.go of its name.
  file(GLOB CLEPSYDRA_TEST_SOURCES CONFIGURE_DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/tests/*_test.cpp)
  add_executable(clepsydra_test tests/main.cpp ${CLEPSYDRA_TEST_SOURCES})
  target_include_directories(clepsydra_test PRIVATE src tests)
  target_link_libraries(clepsydra_test PRIVATE clepsydra)
  target_compile_definitions(clepsydra_test PRIVATE
    CLEPSYDRA_VECTOR_DIR="${CMAKE_CURRENT_SOURCE_DIR}/../spec/vectors"
    CLEPSYDRA_GO_DIR="${CMAKE_CURRENT_SOURCE_DIR}/../go")
  if(NOT MSVC)
    target_compile_options(clepsydra_test PRIVATE -ffp-contract=off -Wall -Wextra)
  endif()

  # ctest (and the test views of IDEs) runs the suite as the sanity tier
  # does: -short, plain and in each mode, then the conformance vectors.
  # test.sh runs every tier.
  enable_testing()
  add_test(NAME suite COMMAND clepsydra_test -short)
  foreach(mode IN ITEMS CT BG NOCAS2)
    add_test(NAME suite-${mode} COMMAND clepsydra_test -short)
    set_tests_properties(suite-${mode} PROPERTIES ENVIRONMENT TF_${mode}=1)
  endforeach()
  add_test(NAME conformance COMMAND clepsydra_test -run ^TestConformance$)

  # go/cmd/tfbench -exact: the scorecard's deterministic metrics (the
  # regression tier diffs them with go/verify/expected/exact.txt).
  add_executable(tfbench tools/tfbench.cpp)
  target_include_directories(tfbench PRIVATE src tests)
  target_link_libraries(tfbench PRIVATE clepsydra)
  if(NOT MSVC)
    target_compile_options(tfbench PRIVATE -ffp-contract=off -Wall -Wextra)
  endif()

  # The perf tier's benchmarks (go/bench_test.go's PERF_BENCH set), printed
  # as go test -bench prints them.
  add_executable(clepsydra_bench bench/bench.cpp)
  target_include_directories(clepsydra_bench PRIVATE src)
  target_link_libraries(clepsydra_bench PRIVATE clepsydra)
  if(NOT MSVC)
    target_compile_options(clepsydra_bench PRIVATE -Wall -Wextra)
  endif()
endif()
