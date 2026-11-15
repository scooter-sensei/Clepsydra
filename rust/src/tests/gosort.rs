//! Go's `sort.Slice`, step for step (go1.24.7 src/sort/zsortfunc.go:
//! pdqsort_func and its helpers). It is not stable: elements that compare
//! equal end in an order the algorithm decides, so a workload that sorts
//! events with tied ticks (velWorkload) replays as in Go only with Go's
//! algorithm.

struct D<'a, T, L: FnMut(&T, &T) -> bool> {
    v: &'a mut [T],
    less: L,
}

impl<T, L: FnMut(&T, &T) -> bool> D<'_, T, L> {
    #[inline]
    fn less(&mut self, i: i64, j: i64) -> bool {
        (self.less)(&self.v[i as usize], &self.v[j as usize])
    }
    #[inline]
    fn swap(&mut self, i: i64, j: i64) {
        self.v.swap(i as usize, j as usize)
    }

    fn insertion_sort(&mut self, a: i64, b: i64) {
        for i in a + 1..b {
            let mut j = i;
            while j > a && self.less(j, j - 1) {
                self.swap(j, j - 1);
                j -= 1;
            }
        }
    }

    fn sift_down(&mut self, lo: i64, hi: i64, first: i64) {
        let mut root = lo;
        loop {
            let mut child = 2 * root + 1;
            if child >= hi {
                break;
            }
            if child + 1 < hi && self.less(first + child, first + child + 1) {
                child += 1;
            }
            if !self.less(first + root, first + child) {
                return;
            }
            self.swap(first + root, first + child);
            root = child;
        }
    }

    fn heap_sort(&mut self, a: i64, b: i64) {
        let (first, lo, hi) = (a, 0, b - a);
        let mut i = (hi - 1) / 2;
        while i >= 0 {
            self.sift_down(i, hi, first);
            i -= 1;
        }
        let mut i = hi - 1;
        while i >= 0 {
            self.swap(first, first + i);
            self.sift_down(lo, i, first);
            i -= 1;
        }
    }

    fn partition(&mut self, a: i64, b: i64, pivot: i64) -> (i64, bool) {
        self.swap(a, pivot);
        let (mut i, mut j) = (a + 1, b - 1);
        while i <= j && self.less(i, a) {
            i += 1;
        }
        while i <= j && !self.less(j, a) {
            j -= 1;
        }
        if i > j {
            self.swap(j, a);
            return (j, true);
        }
        self.swap(i, j);
        i += 1;
        j -= 1;
        loop {
            while i <= j && self.less(i, a) {
                i += 1;
            }
            while i <= j && !self.less(j, a) {
                j -= 1;
            }
            if i > j {
                break;
            }
            self.swap(i, j);
            i += 1;
            j -= 1;
        }
        self.swap(j, a);
        (j, false)
    }

    fn partition_equal(&mut self, a: i64, b: i64, pivot: i64) -> i64 {
        self.swap(a, pivot);
        let (mut i, mut j) = (a + 1, b - 1);
        loop {
            while i <= j && !self.less(a, i) {
                i += 1;
            }
            while i <= j && self.less(a, j) {
                j -= 1;
            }
            if i > j {
                break;
            }
            self.swap(i, j);
            i += 1;
            j -= 1;
        }
        i
    }

    fn partial_insertion_sort(&mut self, a: i64, b: i64) -> bool {
        const MAX_STEPS: i32 = 5;
        const SHORTEST_SHIFTING: i64 = 50;
        let mut i = a + 1;
        for _ in 0..MAX_STEPS {
            while i < b && !self.less(i, i - 1) {
                i += 1;
            }
            if i == b {
                return true;
            }
            if b - a < SHORTEST_SHIFTING {
                return false;
            }
            self.swap(i, i - 1);
            if i - a >= 2 {
                let mut j = i - 1;
                while j >= 1 {
                    if !self.less(j, j - 1) {
                        break;
                    }
                    self.swap(j, j - 1);
                    j -= 1;
                }
            }
            if b - i >= 2 {
                let mut j = i + 1;
                while j < b {
                    if !self.less(j, j - 1) {
                        break;
                    }
                    self.swap(j, j - 1);
                    j += 1;
                }
            }
        }
        false
    }

    fn break_patterns(&mut self, a: i64, b: i64) {
        let length = b - a;
        if length >= 8 {
            let mut random = length as u64; // xorshift(length)
            let modulus = 1u64 << bit_len(length as u64);
            let mid = a + (length / 4) * 2;
            for idx in mid - 1..=mid + 1 {
                random ^= random << 13;
                random ^= random >> 7;
                random ^= random << 17;
                let mut other = (random & (modulus - 1)) as i64;
                if other >= length {
                    other -= length;
                }
                self.swap(idx, a + other);
            }
        }
    }

    fn order2(&mut self, a: i64, b: i64, swaps: &mut i32) -> (i64, i64) {
        if self.less(b, a) {
            *swaps += 1;
            return (b, a);
        }
        (a, b)
    }

    fn median(&mut self, a: i64, b: i64, c: i64, swaps: &mut i32) -> i64 {
        let (a, b) = self.order2(a, b, swaps);
        let (b, _c) = self.order2(b, c, swaps);
        let (_a, b) = self.order2(a, b, swaps);
        b
    }

    fn median_adjacent(&mut self, a: i64, swaps: &mut i32) -> i64 {
        self.median(a - 1, a, a + 1, swaps)
    }

    fn choose_pivot(&mut self, a: i64, b: i64) -> (i64, Hint) {
        const SHORTEST_NINTHER: i64 = 50;
        const MAX_SWAPS: i32 = 4 * 3;
        let l = b - a;
        let mut swaps = 0;
        let (mut i, mut j, mut k) = (a + l / 4, a + l / 4 * 2, a + l / 4 * 3);
        if l >= 8 {
            if l >= SHORTEST_NINTHER {
                i = self.median_adjacent(i, &mut swaps);
                j = self.median_adjacent(j, &mut swaps);
                k = self.median_adjacent(k, &mut swaps);
            }
            j = self.median(i, j, k, &mut swaps);
        }
        if swaps == 0 {
            return (j, Hint::Increasing);
        }
        if swaps == MAX_SWAPS {
            return (j, Hint::Decreasing);
        }
        (j, Hint::Unknown)
    }

    fn reverse_range(&mut self, a: i64, b: i64) {
        let (mut i, mut j) = (a, b - 1);
        while i < j {
            self.swap(i, j);
            i += 1;
            j -= 1;
        }
    }

    fn pdqsort(&mut self, mut a: i64, mut b: i64, mut limit: u32) {
        const MAX_INSERTION: i64 = 12;
        let (mut was_balanced, mut was_partitioned) = (true, true);
        loop {
            let length = b - a;
            if length <= MAX_INSERTION {
                self.insertion_sort(a, b);
                return;
            }
            if limit == 0 {
                self.heap_sort(a, b);
                return;
            }
            if !was_balanced {
                self.break_patterns(a, b);
                limit -= 1;
            }
            let (mut pivot, mut hint) = self.choose_pivot(a, b);
            if hint == Hint::Decreasing {
                self.reverse_range(a, b);
                pivot = (b - 1) - (pivot - a);
                hint = Hint::Increasing;
            }
            if was_balanced && was_partitioned && hint == Hint::Increasing && self.partial_insertion_sort(a, b) {
                return;
            }
            if a > 0 && !self.less(a - 1, pivot) {
                a = self.partition_equal(a, b, pivot);
                continue;
            }
            let (mid, already) = self.partition(a, b, pivot);
            was_partitioned = already;
            let (left_len, right_len) = (mid - a, b - mid);
            let balance_threshold = length / 8;
            if left_len < right_len {
                was_balanced = left_len >= balance_threshold;
                self.pdqsort(a, mid, limit);
                a = mid + 1;
            } else {
                was_balanced = right_len >= balance_threshold;
                self.pdqsort(mid + 1, b, limit);
                b = mid;
            }
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Hint {
    Unknown,
    Increasing,
    Decreasing,
}

/// `bits.Len`.
fn bit_len(x: u64) -> u32 {
    64 - x.leading_zeros()
}

/// `sort.Slice(v, less)`: less compares two elements.
pub(crate) fn slice<T>(v: &mut [T], less: impl FnMut(&T, &T) -> bool) {
    let n = v.len() as i64;
    let mut d = D { v, less };
    d.pdqsort(0, n, bit_len(n as u64));
}
