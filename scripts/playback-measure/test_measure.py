import unittest
from fractions import Fraction
from analyze import summarize
from seek import errors, frames


class MeasurementTests(unittest.TestCase):
    def test_visibility_uses_each_write_completion_and_first_sufficient_read(self):
        writes = [{"seq": 0, "size": 10, "end": 100_000_000},
                  {"seq": 1, "size": 20, "end": 200_000_000}]
        reads = [{"stat": 10, "fstat": 10, "read": 10,
                  "stat_end": 150_000_000, "end": 160_000_000},
                 {"stat": 20, "fstat": 20, "read": 20,
                  "stat_end": 260_000_000, "end": 280_000_000}]
        result = summarize(writes, reads, 20_000_000)
        self.assertEqual(result["stat"]["median_ms"], 75)
        self.assertEqual(result["read"]["median_ms"], 90)
        self.assertEqual(result["read"]["max_ms"], 100)
        with self.assertRaisesRegex(ValueError, "unobserved"):
            summarize(writes, reads[:1], 0)

    def test_frame_match_uses_input_grid_and_rejects_ambiguous_or_missing_hashes(self):
        reference = [(Fraction(i, 25), str(i)) for i in range(200)]
        output = [(Fraction(0), "26"), (Fraction(2), "76")]
        self.assertEqual([r["error_frames"] for r in
                          errors(reference, output, Fraction(103, 100), Fraction(2))], [0, 0])
        shifted = [(Fraction(0), "27"), (Fraction(2), "77")]
        self.assertEqual([r["error_frames"] for r in
                          errors(reference, shifted, Fraction(103, 100), Fraction(2))], [1, 1])
        for invalid in (reference + [(Fraction(100), "26")], reference[30:]):
            with self.assertRaisesRegex(ValueError, "matching source frames"):
                errors(invalid, output, Fraction(103, 100), Fraction(2))

    def test_framehash_timebase_is_rational(self):
        self.assertEqual(frames("#tb 0: 1001/30000\n0, 5, 5, 1, 100, abc\n"),
                         [(Fraction(1001, 6000), "abc")])


if __name__ == "__main__":
    unittest.main()
