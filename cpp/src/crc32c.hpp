// CRC32C (Castagnoli), as Go's hash/crc32 computes it with
// crc32.MakeTable(crc32.Castagnoli): the snapshot's check words.
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>

namespace clepsydra::detail {

inline const std::array<std::uint32_t, 256>& crc32cTable() {
  static const std::array<std::uint32_t, 256> t = [] {
    std::array<std::uint32_t, 256> x{};
    for (std::uint32_t i = 0; i < 256; i++) {
      std::uint32_t c = i;
      for (int k = 0; k < 8; k++) c = (c & 1) ? (c >> 1) ^ 0x82F63B78u : c >> 1;
      x[i] = c;
    }
    return x;
  }();
  return t;
}

// crc32cUpdate is crc32.Update(crc, castagnoli, p[:n]).
inline std::uint32_t crc32cUpdate(std::uint32_t crc, const std::uint8_t* p, std::size_t n) {
  const auto& t = crc32cTable();
  crc = ~crc;
  for (std::size_t i = 0; i < n; i++) crc = t[(crc ^ p[i]) & 0xff] ^ (crc >> 8);
  return ~crc;
}

}  // namespace clepsydra::detail
