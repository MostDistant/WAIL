// Runner for the WAIL plugin integration tests (see test_framework.h).
#include <cstdio>

#include "test_framework.h"

int main() {
   int failed = 0;
   for (auto &tc : wailtest::registry()) {
      std::fprintf(stderr, "[ RUN  ] %s\n", tc.name);
      int before = wailtest::failureCount();
      tc.fn();
      if (wailtest::failureCount() == before) {
         std::fprintf(stderr, "[  OK  ] %s\n", tc.name);
      } else {
         std::fprintf(stderr, "[ FAIL ] %s\n", tc.name);
         failed++;
      }
   }
   std::fprintf(stderr, "%zu test(s), %d failed\n", wailtest::registry().size(), failed);
   return failed ? 1 : 0;
}
