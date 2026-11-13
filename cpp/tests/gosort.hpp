// Go's sort.Slice, step for step (go1.24.7 src/sort/zsortfunc.go: pdqsort_func
// and its helpers). It is not stable: elements that compare equal end in an
// order the algorithm decides, so a workload that sorts events with tied ticks
// (velWorkload) replays as in Go only with Go's algorithm.
#pragma once

#include <bit>
#include <cstdint>
#include <tuple>
#include <utility>

namespace gosort {

template <class Less, class Swap>
struct LessSwap {
  Less less;
  Swap swap;
  bool Less_(std::int64_t i, std::int64_t j) { return less(i, j); }
  void Swap_(std::int64_t i, std::int64_t j) { swap(i, j); }
};

enum Hint { unknownHint, increasingHint, decreasingHint };

template <class D>
void insertionSort(D& d, std::int64_t a, std::int64_t b) {
  for (std::int64_t i = a + 1; i < b; i++)
    for (std::int64_t j = i; j > a && d.Less_(j, j - 1); j--) d.Swap_(j, j - 1);
}

template <class D>
void siftDown(D& d, std::int64_t lo, std::int64_t hi, std::int64_t first) {
  std::int64_t root = lo;
  for (;;) {
    std::int64_t child = 2 * root + 1;
    if (child >= hi) break;
    if (child + 1 < hi && d.Less_(first + child, first + child + 1)) child++;
    if (!d.Less_(first + root, first + child)) return;
    d.Swap_(first + root, first + child);
    root = child;
  }
}

template <class D>
void heapSort(D& d, std::int64_t a, std::int64_t b) {
  std::int64_t first = a, lo = 0, hi = b - a;
  for (std::int64_t i = (hi - 1) / 2; i >= 0; i--) siftDown(d, i, hi, first);
  for (std::int64_t i = hi - 1; i >= 0; i--) {
    d.Swap_(first, first + i);
    siftDown(d, lo, i, first);
  }
}

template <class D>
std::pair<std::int64_t, bool> partition(D& d, std::int64_t a, std::int64_t b, std::int64_t pivot) {
  d.Swap_(a, pivot);
  std::int64_t i = a + 1, j = b - 1;
  while (i <= j && d.Less_(i, a)) i++;
  while (i <= j && !d.Less_(j, a)) j--;
  if (i > j) {
    d.Swap_(j, a);
    return {j, true};
  }
  d.Swap_(i, j);
  i++;
  j--;
  for (;;) {
    while (i <= j && d.Less_(i, a)) i++;
    while (i <= j && !d.Less_(j, a)) j--;
    if (i > j) break;
    d.Swap_(i, j);
    i++;
    j--;
  }
  d.Swap_(j, a);
  return {j, false};
}

template <class D>
std::int64_t partitionEqual(D& d, std::int64_t a, std::int64_t b, std::int64_t pivot) {
  d.Swap_(a, pivot);
  std::int64_t i = a + 1, j = b - 1;
  for (;;) {
    while (i <= j && !d.Less_(a, i)) i++;
    while (i <= j && d.Less_(a, j)) j--;
    if (i > j) break;
    d.Swap_(i, j);
    i++;
    j--;
  }
  return i;
}

template <class D>
bool partialInsertionSort(D& d, std::int64_t a, std::int64_t b) {
  const int maxSteps = 5;
  const std::int64_t shortestShifting = 50;
  std::int64_t i = a + 1;
  for (int step = 0; step < maxSteps; step++) {
    while (i < b && !d.Less_(i, i - 1)) i++;
    if (i == b) return true;
    if (b - a < shortestShifting) return false;
    d.Swap_(i, i - 1);
    if (i - a >= 2) {
      for (std::int64_t j = i - 1; j >= 1; j--) {
        if (!d.Less_(j, j - 1)) break;
        d.Swap_(j, j - 1);
      }
    }
    if (b - i >= 2) {
      for (std::int64_t j = i + 1; j < b; j++) {
        if (!d.Less_(j, j - 1)) break;
        d.Swap_(j, j - 1);
      }
    }
  }
  return false;
}

template <class D>
void breakPatterns(D& d, std::int64_t a, std::int64_t b) {
  std::int64_t length = b - a;
  if (length >= 8) {
    std::uint64_t random = static_cast<std::uint64_t>(length);  // xorshift(length)
    std::uint64_t modulus = std::uint64_t{1} << std::bit_width(static_cast<std::uint64_t>(length));
    for (std::int64_t idx = a + (length / 4) * 2 - 1; idx <= a + (length / 4) * 2 + 1; idx++) {
      random ^= random << 13;
      random ^= random >> 7;
      random ^= random << 17;
      std::int64_t other = static_cast<std::int64_t>(random & (modulus - 1));
      if (other >= length) other -= length;
      d.Swap_(idx, a + other);
    }
  }
}

template <class D>
std::pair<std::int64_t, std::int64_t> order2(D& d, std::int64_t a, std::int64_t b, int& swaps) {
  if (d.Less_(b, a)) {
    swaps++;
    return {b, a};
  }
  return {a, b};
}

template <class D>
std::int64_t median(D& d, std::int64_t a, std::int64_t b, std::int64_t c, int& swaps) {
  std::tie(a, b) = order2(d, a, b, swaps);
  std::tie(b, c) = order2(d, b, c, swaps);
  std::tie(a, b) = order2(d, a, b, swaps);
  return b;
}

template <class D>
std::int64_t medianAdjacent(D& d, std::int64_t a, int& swaps) {
  return median(d, a - 1, a, a + 1, swaps);
}

template <class D>
std::pair<std::int64_t, Hint> choosePivot(D& d, std::int64_t a, std::int64_t b) {
  const std::int64_t shortestNinther = 50;
  const int maxSwaps = 4 * 3;
  std::int64_t l = b - a;
  int swaps = 0;
  std::int64_t i = a + l / 4 * 1, j = a + l / 4 * 2, k = a + l / 4 * 3;
  if (l >= 8) {
    if (l >= shortestNinther) {
      i = medianAdjacent(d, i, swaps);
      j = medianAdjacent(d, j, swaps);
      k = medianAdjacent(d, k, swaps);
    }
    j = median(d, i, j, k, swaps);
  }
  if (swaps == 0) return {j, increasingHint};
  if (swaps == maxSwaps) return {j, decreasingHint};
  return {j, unknownHint};
}

template <class D>
void reverseRange(D& d, std::int64_t a, std::int64_t b) {
  for (std::int64_t i = a, j = b - 1; i < j; i++, j--) d.Swap_(i, j);
}

template <class D>
void pdqsort(D& d, std::int64_t a, std::int64_t b, int limit) {
  const std::int64_t maxInsertion = 12;
  bool wasBalanced = true, wasPartitioned = true;
  for (;;) {
    std::int64_t length = b - a;
    if (length <= maxInsertion) {
      insertionSort(d, a, b);
      return;
    }
    if (limit == 0) {
      heapSort(d, a, b);
      return;
    }
    if (!wasBalanced) {
      breakPatterns(d, a, b);
      limit--;
    }
    auto [pivot, hint] = choosePivot(d, a, b);
    if (hint == decreasingHint) {
      reverseRange(d, a, b);
      pivot = (b - 1) - (pivot - a);
      hint = increasingHint;
    }
    if (wasBalanced && wasPartitioned && hint == increasingHint) {
      if (partialInsertionSort(d, a, b)) return;
    }
    if (a > 0 && !d.Less_(a - 1, pivot)) {
      a = partitionEqual(d, a, b, pivot);
      continue;
    }
    auto [mid, already] = partition(d, a, b, pivot);
    wasPartitioned = already;
    std::int64_t leftLen = mid - a, rightLen = b - mid;
    std::int64_t balanceThreshold = length / 8;
    if (leftLen < rightLen) {
      wasBalanced = leftLen >= balanceThreshold;
      pdqsort(d, a, mid, limit);
      a = mid + 1;
    } else {
      wasBalanced = rightLen >= balanceThreshold;
      pdqsort(d, mid + 1, b, limit);
      b = mid;
    }
  }
}

// Slice is sort.Slice(x, less) for a container v: less(i, j) compares
// v[i] and v[j].
template <class V, class Less>
void Slice(V& v, Less less) {
  auto lessIdx = [&](std::int64_t i, std::int64_t j) {
    return less(v[static_cast<std::size_t>(i)], v[static_cast<std::size_t>(j)]);
  };
  auto swapIdx = [&](std::int64_t i, std::int64_t j) {
    std::swap(v[static_cast<std::size_t>(i)], v[static_cast<std::size_t>(j)]);
  };
  LessSwap<decltype(lessIdx), decltype(swapIdx)> d{lessIdx, swapIdx};
  std::int64_t n = static_cast<std::int64_t>(v.size());
  pdqsort(d, 0, n, std::bit_width(static_cast<std::uint64_t>(n)));
}

}  // namespace gosort
