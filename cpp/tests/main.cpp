// The test runner: clepsydra_test [-run REGEX] [-v] [-short] [-count N].
// Environment switches as the Go suite's: TF_CT=1 (every filter
// ConstantTime), TF_BG=1 (every filter Background), TF_NOCAS2=1 (realign's
// hold instead of its 16-byte CAS). -short is go test -short (the long tests
// run fewer rounds); -count N runs each selected test N times (go test
// -count), every repeat whatever the others did, as go test runs them.
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <regex>

#include "check.hpp"
#include "core.hpp"

namespace {
bool envOn(const char* name) {
  const char* v = std::getenv(name);
  return v && *v;  // Go: os.Getenv(name) != "" (so TF_CT=0 is on, as there)
}
}  // namespace

int main(int argc, char** argv) {
  std::string pattern = ".*";
  int count = 1;
  for (int i = 1; i < argc; i++) {
    if (std::strcmp(argv[i], "-run") == 0 && i + 1 < argc) {
      pattern = argv[++i];
    } else if (std::strcmp(argv[i], "-v") == 0) {
      check::verbose() = true;
    } else if (std::strcmp(argv[i], "-short") == 0) {
      check::short_() = true;
    } else if (std::strcmp(argv[i], "-count") == 0 && i + 1 < argc) {
      count = std::max(1, std::atoi(argv[++i]));
    } else {
      std::fprintf(stderr, "usage: %s [-run REGEX] [-v] [-short] [-count N]\n", argv[0]);
      return 2;
    }
  }
  clepsydra::detail::forceCT = envOn("TF_CT");
  clepsydra::detail::forceBG = envOn("TF_BG");
  if (envOn("TF_NOCAS2")) clepsydra::detail::cpu::HasCAS2 = false;
  std::regex re(pattern);
  int ran = 0, failedTests = 0, skipped = 0;
  for (auto& t : check::registry()) {
    if (!std::regex_search(t.name, re)) continue;
    for (int c = 0; c < count; c++) {
      ran++;
      int before = check::failures();
      auto t0 = std::chrono::steady_clock::now();
      if (check::verbose()) std::printf("=== RUN   %s\n", t.name);
      std::fflush(stdout);
      bool skip = false;
      try {
        t.fn();
      } catch (const check::Failed&) {
      } catch (const check::Skipped&) {
        skip = true;
      } catch (const std::exception& e) {
        check::fail(__FILE__, __LINE__, std::string("uncaught exception: ") + e.what());
      }
      double secs = std::chrono::duration<double>(std::chrono::steady_clock::now() - t0).count();
      bool failed = check::failures() > before;
      if (failed) {
        failedTests++;
        std::printf("--- FAIL: %s (%.2fs)\n", t.name, secs);
      } else if (skip) {
        skipped++;
        if (check::verbose()) std::printf("--- SKIP: %s (%.2fs)\n", t.name, secs);
      } else if (check::verbose()) {
        std::printf("--- PASS: %s (%.2fs)\n", t.name, secs);
      }
      std::fflush(stdout);
      if (skip) break;  // a skip repeats as a skip
    }
  }
  if (ran == 0) {
    std::printf("no tests match %s\n", pattern.c_str());
    return 1;
  }
  std::printf("%s: %d run, %d failed, %d skipped\n", failedTests ? "FAIL" : "ok", ran, failedTests, skipped);
  return failedTests ? 1 : 0;
}
