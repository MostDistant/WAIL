// Minimal test framework for the WAIL plugin integration tests. Hand-rolled on
// purpose: the harness's only external dependency should be clap-trap itself (see
// tradeoffs.md). Tests self-register via the TEST macro; main.cpp runs them all.
#pragma once

#include <cstdio>
#include <string>
#include <vector>

namespace wailtest {

struct TestCase {
   const char *name;
   void (*fn)();
};

inline std::vector<TestCase> &registry() {
   static std::vector<TestCase> r;
   return r;
}

struct Registrar {
   Registrar(const char *name, void (*fn)()) { registry().push_back({name, fn}); }
};

inline int &failureCount() {
   static int n = 0;
   return n;
}

inline void fail(int line, const std::string &msg) {
   failureCount()++;
   std::fprintf(stderr, "    FAIL (line %d): %s\n", line, msg.c_str());
}

} // namespace wailtest

#define TEST(name)                                                                   \
   static void test_##name();                                                        \
   static ::wailtest::Registrar registrar_##name(#name, test_##name);                \
   static void test_##name()

#define CHECK(cond)                                                                  \
   do {                                                                              \
      if (!(cond)) ::wailtest::fail(__LINE__, "CHECK(" #cond ")");                   \
   } while (0)

#define CHECK_MSG(cond, msg)                                                         \
   do {                                                                              \
      if (!(cond)) ::wailtest::fail(__LINE__, std::string("CHECK(" #cond "): ") + (msg)); \
   } while (0)
