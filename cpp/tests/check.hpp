// A minimal test harness, Go's testing package in miniature, so the port's
// tests build and run offline with nothing but the compiler: TEST registers a
// test, CHECK, CHECK_EQ and ERRORF record a failure and continue (t.Errorf),
// REQUIRE and FATALF stop the test (t.Fatalf), LOGF logs under -v (t.Logf),
// SKIP skips it (t.Skip). tests/main.cpp runs them:
// clepsydra_test [-run REGEX] [-v] [-short] [-count N].
//
// ERRORF, FATALF and LOGF take Go format strings ("%d", "%v", "%+v", "%#x",
// "%.2e", "%7.4f", "%q"): each verb prints its argument with operator<<, so a
// t.Fatalf line ports as it is. Failures may be recorded from any thread
// (t.Errorf is goroutine-safe); FATALF, REQUIRE and SKIP only from the test's
// own thread (as t.Fatalf only from the test's goroutine).
#pragma once

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <iomanip>
#include <mutex>
#include <sstream>
#include <string>
#include <string_view>
#include <type_traits>
#include <utility>
#include <vector>

namespace check {

struct Test {
  const char* name;
  std::function<void()> fn;
};

inline std::vector<Test>& registry() {
  static std::vector<Test> r;
  return r;
}

struct Register {
  Register(const char* name, std::function<void()> fn) { registry().push_back({name, std::move(fn)}); }
};

struct Failed {};   // thrown by REQUIRE, FATALF
struct Skipped {};  // thrown by SKIP

inline std::atomic<int>& failures() {
  static std::atomic<int> n{0};
  return n;
}
inline bool& verbose() {
  static bool v = false;
  return v;
}
// short_ is go test -short: the long tests run fewer rounds.
inline bool& short_() {
  static bool s = false;
  return s;
}
inline std::mutex& outMu() {
  static std::mutex m;
  return m;
}

inline void fail(const char* file, int line, const std::string& msg) {
  failures()++;
  std::lock_guard<std::mutex> lk(outMu());
  std::fprintf(stdout, "    %s:%d: %s\n", file, line, msg.c_str());
  std::fflush(stdout);
}
inline void log(const std::string& msg) {
  if (!verbose()) return;
  std::lock_guard<std::mutex> lk(outMu());
  std::fprintf(stdout, "    %s\n", msg.c_str());
}
// print writes a line whatever -v says (the experiments' tables, which the
// regression tier compares with go/verify/expected/).
inline void print(const std::string& msg) {
  std::lock_guard<std::mutex> lk(outMu());
  std::fprintf(stdout, "%s\n", msg.c_str());
  std::fflush(stdout);
}

// ---- Go-style formatting ----

template <class T, class = void>
struct isRange : std::false_type {};
template <class T>
struct isRange<
    T, std::void_t<decltype(std::begin(std::declval<const T&>())), decltype(std::end(std::declval<const T&>()))>>
    : std::true_type {};

struct Spec {
  bool alt = false, plus = false, zero = false, left = false, space = false;
  int width = -1, prec = -1;
  char verb = 'v';
};

template <class T>
void putValue(std::ostream& o, const Spec& s, const T& v);

// goDuration is Go's time.Duration.String: "1m0s", "1.5s", "250ms", "0s".
inline std::string goDuration(std::int64_t d) {
  if (d == 0) return "0s";
  const bool neg = d < 0;
  std::uint64_t u = neg ? ~static_cast<std::uint64_t>(d) + 1 : static_cast<std::uint64_t>(d);
  // frac: the fraction of u / 10^prec without trailing zeros (".25"), and u / 10^prec.
  auto frac = [](std::uint64_t& v, int prec) {
    std::string f;
    bool print = false;
    for (int i = 0; i < prec; i++) {
      std::uint64_t digit = v % 10;
      print = print || digit != 0;
      if (print) f.insert(f.begin(), static_cast<char>('0' + digit));
      v /= 10;
    }
    return print ? "." + f : f;
  };
  std::string s;
  if (u < 1'000'000'000) {
    int prec;
    std::string unit;
    if (u < 1'000) {
      prec = 0, unit = "ns";
    } else if (u < 1'000'000) {
      prec = 3, unit = "\xC2\xB5s";  // µs
    } else {
      prec = 6, unit = "ms";
    }
    std::string f = frac(u, prec);
    s = std::to_string(u) + f + unit;
  } else {
    std::string f = frac(u, 9);
    s = std::to_string(u % 60) + f + "s";
    u /= 60;
    if (u > 0) {
      s = std::to_string(u % 60) + "m" + s;
      u /= 60;
      if (u > 0) s = std::to_string(u) + "h" + s;
    }
  }
  return neg ? "-" + s : s;
}

template <class T>
struct isDuration : std::false_type {};
template <class R, class P>
struct isDuration<std::chrono::duration<R, P>> : std::true_type {};

template <class T>
void putPlain(std::ostream& o, const Spec& s, const T& v) {
  using U = std::decay_t<T>;
  if constexpr (std::is_same_v<U, bool>) {
    o << (v ? "true" : "false");
  } else if constexpr (isDuration<U>::value) {
    o << goDuration(static_cast<std::int64_t>(std::chrono::duration_cast<std::chrono::nanoseconds>(v).count()));
  } else if constexpr (std::is_same_v<U, std::string> || std::is_same_v<U, const char*> || std::is_same_v<U, char*> ||
                       std::is_convertible_v<U, std::string_view>) {
    if (s.verb == 'q') {
      o << '"' << v << '"';
    } else {
      o << v;
    }
  } else if constexpr (isRange<U>::value) {
    o << '[';
    bool first = true;
    for (const auto& x : v) {
      if (!first) o << ' ';
      first = false;
      putValue(o, s, x);
    }
    o << ']';
  } else if constexpr (std::is_integral_v<U>) {
    if (s.verb == 'x' || s.verb == 'X') {
      if (s.alt) o << "0x";
      o << std::hex << (s.verb == 'X' ? std::uppercase : std::nouppercase)
        << static_cast<unsigned long long>(static_cast<std::make_unsigned_t<U>>(v)) << std::dec << std::nouppercase;
    } else if (s.verb == 'b') {
      unsigned long long x = static_cast<unsigned long long>(static_cast<std::make_unsigned_t<U>>(v));
      std::string b;
      do {
        b.insert(b.begin(), static_cast<char>('0' + (x & 1)));
        x >>= 1;
      } while (x);
      o << b;
    } else if (s.verb == 'c') {
      o << static_cast<char>(v);
    } else {
      if (s.plus && v >= 0) o << '+';
      if constexpr (sizeof(U) == 1) {
        o << static_cast<int>(v);
      } else {
        o << v;
      }
    }
  } else if constexpr (std::is_floating_point_v<U>) {
    if (std::isnan(static_cast<double>(v))) {  // Go: NaN, +Inf and -Inf whatever the verb
      o << "NaN";
      return;
    }
    if (std::isinf(static_cast<double>(v))) {
      o << (v > 0 ? "+Inf" : "-Inf");
      return;
    }
    if (s.plus && v >= 0) o << '+';
    std::ostringstream x;
    if (s.verb == 'e' || s.verb == 'E') {
      x << std::scientific << std::setprecision(s.prec < 0 ? 6 : s.prec) << v;
      // Go writes the exponent with at least two digits, as C++ does.
    } else if (s.verb == 'f' || s.verb == 'F') {
      x << std::fixed << std::setprecision(s.prec < 0 ? 6 : s.prec) << v;
    } else if (s.verb == 'g' || s.verb == 'G') {
      if (s.prec >= 0) x << std::setprecision(s.prec);
      x << v;
    } else {  // %v: strconv's 'g' with precision -1
      // The shortest digits that read back; then, as strconv decides for the
      // shortest, %e when the exponent is < -4 or >= 6 (not C's %g rule,
      // which compares with the digit count: Go prints 1e+06 and 100000,
      // C's shortest %g 1e+06 and 1e+05). Exponents have two digits or more
      // in both.
      const double d = static_cast<double>(v);
      char buf[64];
      int p = 1;
      for (; p <= 17; p++) {
        std::snprintf(buf, sizeof buf, "%.*e", p - 1, d);
        if (std::strtod(buf, nullptr) == d) break;
      }
      int exp = std::atoi(std::strchr(buf, 'e') + 1);
      if (d != 0 && (exp < -4 || exp >= 6)) {
        x << buf;
      } else {
        std::snprintf(buf, sizeof buf, "%.*f", std::max(p - 1 - exp, 0), d);
        x << buf;
      }
    }
    o << x.str();
  } else if constexpr (std::is_pointer_v<U>) {
    o << static_cast<const void*>(v);
  } else {
    o << v;
  }
}

template <class T>
void putValue(std::ostream& o, const Spec& s, const T& v) {
  putPlain(o, s, v);
}

using Putter = std::function<void(std::ostream&, const Spec&)>;

template <class T>
Putter putter(const T& v) {
  return [&v](std::ostream& o, const Spec& s) { putValue(o, s, v); };
}

inline std::string formatv(const char* f, const std::vector<Putter>& args) {
  std::ostringstream out;
  std::size_t ai = 0;
  for (const char* p = f; *p; p++) {
    if (*p != '%') {
      out << *p;
      continue;
    }
    p++;
    if (*p == '%') {
      out << '%';
      continue;
    }
    Spec s;
    for (;; p++) {
      if (*p == '#')
        s.alt = true;
      else if (*p == '+')
        s.plus = true;
      else if (*p == '0')
        s.zero = true;
      else if (*p == '-')
        s.left = true;
      else if (*p == ' ')
        s.space = true;
      else
        break;
    }
    if (*p >= '0' && *p <= '9') {
      s.width = 0;
      while (*p >= '0' && *p <= '9') s.width = s.width * 10 + (*p++ - '0');
    }
    if (*p == '.') {
      p++;
      s.prec = 0;
      while (*p >= '0' && *p <= '9') s.prec = s.prec * 10 + (*p++ - '0');
    }
    s.verb = *p;
    if (!*p) break;
    std::ostringstream one;
    if (ai < args.size()) {
      args[ai++](one, s);
    } else {
      one << "%!" << s.verb << "(MISSING)";
    }
    std::string str = one.str();
    if (s.width > 0 && static_cast<int>(str.size()) < s.width) {
      std::size_t padN = static_cast<std::size_t>(s.width) - str.size();
      if (s.left) {
        str.append(padN, ' ');
      } else if (s.zero && s.verb != 's' && s.verb != 'v' && s.verb != 'q') {
        std::size_t at = (!str.empty() && (str[0] == '-' || str[0] == '+')) ? 1 : 0;
        str.insert(at, padN, '0');
      } else {
        str.insert(0, padN, ' ');
      }
    }
    out << str;
  }
  return out.str();
}

// format is fmt.Sprintf for the verbs the suite uses.
template <class... A>
std::string format(const char* f, const A&... a) {
  return formatv(f, std::vector<Putter>{putter(a)...});
}

}  // namespace check

#define CHECK_CAT2(a, b) a##b
#define CHECK_CAT(a, b) CHECK_CAT2(a, b)
#define TEST(name)                                                    \
  static void name();                                                 \
  static ::check::Register CHECK_CAT(check_reg_, name)(#name, &name); \
  static void name()

#define CHECK(cond)                                                         \
  do {                                                                      \
    if (!(cond)) ::check::fail(__FILE__, __LINE__, "check failed: " #cond); \
  } while (0)

#define CHECK_MSG(cond, msg)                               \
  do {                                                     \
    if (!(cond)) ::check::fail(__FILE__, __LINE__, (msg)); \
  } while (0)

#define CHECK_EQ(got, want)                                                                        \
  do {                                                                                             \
    auto check_g = (got);                                                                          \
    auto check_w = (want);                                                                         \
    if (!(check_g == check_w))                                                                     \
      ::check::fail(__FILE__, __LINE__, ::check::format(#got " = %v, want %v", check_g, check_w)); \
  } while (0)

#define REQUIRE(cond)                                        \
  do {                                                       \
    if (!(cond)) {                                           \
      ::check::fail(__FILE__, __LINE__, "required: " #cond); \
      throw ::check::Failed{};                               \
    }                                                        \
  } while (0)

// ERRORF is t.Errorf, FATALF t.Fatalf, LOGF t.Logf.
#define ERRORF(...) ::check::fail(__FILE__, __LINE__, ::check::format(__VA_ARGS__))
#define FATALF(...)                                                  \
  do {                                                               \
    ::check::fail(__FILE__, __LINE__, ::check::format(__VA_ARGS__)); \
    throw ::check::Failed{};                                         \
  } while (0)
#define LOGF(...) ::check::log(::check::format(__VA_ARGS__))

#define SKIP(msg)             \
  do {                        \
    ::check::log(msg);        \
    throw ::check::Skipped{}; \
  } while (0)
