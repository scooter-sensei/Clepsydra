// A small JSON reader for the conformance vectors (spec/vectors/*.json):
// objects, arrays, strings with escapes, numbers kept as their text (so a
// uint64 such as 18446744073709551615 reads exactly), true, false, null.
#pragma once

#include <cctype>
#include <cstdint>
#include <map>
#include <memory>
#include <stdexcept>
#include <string>
#include <vector>

namespace json {

struct Value {
  enum Kind { Null, Bool, Number, String, Array, Object } kind = Null;
  bool b = false;
  std::string s;  // a string's value, or a number's text
  std::vector<Value> a;
  std::map<std::string, Value> o;

  bool has(const std::string& k) const { return kind == Object && o.count(k) != 0; }
  const Value& operator[](const std::string& k) const {
    auto it = o.find(k);
    if (it == o.end()) throw std::runtime_error("json: no key " + k);
    return it->second;
  }
  std::uint64_t u64() const {
    if (kind != Number) throw std::runtime_error("json: not a number");
    return std::stoull(s);
  }
  std::int64_t i64() const {
    if (kind != Number) throw std::runtime_error("json: not a number");
    return std::stoll(s);
  }
  double f64() const {
    if (kind != Number) throw std::runtime_error("json: not a number");
    return std::stod(s);
  }
  // flag is an optional boolean field: a missing one is false.
  bool flag(const std::string& k) const { return has(k) && (*this)[k].b; }
};

class Parser {
 public:
  explicit Parser(const std::string& text) : t_(text) {}
  Value parse() {
    Value v = value();
    ws();
    if (i_ != t_.size()) err("trailing text");
    return v;
  }

 private:
  [[noreturn]] void err(const char* what) {
    throw std::runtime_error(std::string("json: ") + what + " at offset " + std::to_string(i_));
  }
  void ws() {
    while (i_ < t_.size() && (t_[i_] == ' ' || t_[i_] == '\n' || t_[i_] == '\r' || t_[i_] == '\t')) i_++;
  }
  bool lit(const char* w) {
    std::size_t n = std::char_traits<char>::length(w);
    if (t_.compare(i_, n, w) == 0) {
      i_ += n;
      return true;
    }
    return false;
  }
  Value value() {
    ws();
    if (i_ >= t_.size()) err("unexpected end");
    Value v;
    char c = t_[i_];
    if (c == '{') {
      v.kind = Value::Object;
      i_++;
      ws();
      if (t_[i_] == '}') {
        i_++;
        return v;
      }
      for (;;) {
        ws();
        std::string k = str();
        ws();
        if (t_[i_++] != ':') err("expected :");
        v.o[k] = value();
        ws();
        if (t_[i_] == ',') {
          i_++;
          continue;
        }
        if (t_[i_++] != '}') err("expected }");
        return v;
      }
    }
    if (c == '[') {
      v.kind = Value::Array;
      i_++;
      ws();
      if (t_[i_] == ']') {
        i_++;
        return v;
      }
      for (;;) {
        v.a.push_back(value());
        ws();
        if (t_[i_] == ',') {
          i_++;
          continue;
        }
        if (t_[i_++] != ']') err("expected ]");
        return v;
      }
    }
    if (c == '"') {
      v.kind = Value::String;
      v.s = str();
      return v;
    }
    if (lit("true")) {
      v.kind = Value::Bool;
      v.b = true;
      return v;
    }
    if (lit("false")) {
      v.kind = Value::Bool;
      return v;
    }
    if (lit("null")) return v;
    std::size_t s = i_;
    while (i_ < t_.size() && (std::isdigit(static_cast<unsigned char>(t_[i_])) || t_[i_] == '-' || t_[i_] == '+' ||
                              t_[i_] == '.' || t_[i_] == 'e' || t_[i_] == 'E'))
      i_++;
    if (s == i_) err("unexpected character");
    v.kind = Value::Number;
    v.s = t_.substr(s, i_ - s);
    return v;
  }
  std::string str() {
    if (t_[i_++] != '"') err("expected string");
    std::string out;
    while (i_ < t_.size() && t_[i_] != '"') {
      char c = t_[i_++];
      if (c != '\\') {
        out += c;
        continue;
      }
      char e = t_[i_++];
      switch (e) {
        case '"': out += '"'; break;
        case '\\': out += '\\'; break;
        case '/': out += '/'; break;
        case 'b': out += '\b'; break;
        case 'f': out += '\f'; break;
        case 'n': out += '\n'; break;
        case 'r': out += '\r'; break;
        case 't': out += '\t'; break;
        case 'u': {
          unsigned cp = static_cast<unsigned>(std::stoul(t_.substr(i_, 4), nullptr, 16));
          i_ += 4;
          if (cp < 0x80) {
            out += static_cast<char>(cp);
          } else if (cp < 0x800) {
            out += static_cast<char>(0xC0 | (cp >> 6));
            out += static_cast<char>(0x80 | (cp & 0x3F));
          } else {
            out += static_cast<char>(0xE0 | (cp >> 12));
            out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
            out += static_cast<char>(0x80 | (cp & 0x3F));
          }
          break;
        }
        default: err("bad escape");
      }
    }
    if (i_ >= t_.size()) err("unterminated string");
    i_++;
    return out;
  }
  const std::string& t_;
  std::size_t i_ = 0;
};

inline Value parse(const std::string& text) { return Parser(text).parse(); }

}  // namespace json
