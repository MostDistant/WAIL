// Compilation unit for the vendored Ableton `abl_link` C wrapper.
//
// cgo compiles every .cpp in the package directory with the C++ toolchain, so we
// pull in the official wrapper (which instantiates the header-only Link SDK) here
// rather than copying it. Include paths and platform defines come from the cgo
// directives in abllink.go.
//
// Windows/MinGW: asio wants <winsock2.h> included before <windows.h>. NOMINMAX
// avoids the min/max macros.
//
// Separately, MinGW's COM header #defines `interface` as a macro, which collides
// with the `interface` parameter in Link's link_audio/Channels.hpp. Undef-ing
// the macro from here proved unreliable (windows.h doesn't pull the definer on
// MinGW; asio pulls it after our #undef), so that collision is fixed
// deterministically by renaming the parameter in the vendored header at build
// time — see the "Rename Link `interface` param for MinGW" step in the Windows
// CI jobs and DEVELOPMENT.md. (macOS/Linux need neither.)
#if defined(_WIN32)
#define NOMINMAX
#include <winsock2.h>
#include <windows.h>
#else
#include <time.h>
#endif

#include "abl_link.cpp"

// wail_mono_micros is the machine monotonic clock WAIL shares with the CLAP
// plugins (wail_mono_micros in wail_ipc.h — same libc calls, same domain by
// construction). The app converts Link-timeline chunk stamps into this domain
// so a plugin can render them against its host sample clock. It deliberately
// does NOT use Link's clock: the session timeline is offset and filtered
// (converges across peers), while this must match what the plugin can read.
extern "C" int64_t wail_mono_micros() {
#if defined(_WIN32)
  static const long long freq = [] {
    LARGE_INTEGER f;
    QueryPerformanceFrequency(&f);
    return f.QuadPart;
  }();
  LARGE_INTEGER now;
  QueryPerformanceCounter(&now);
  return (int64_t)(now.QuadPart / (freq / 1000000.0));
#elif defined(__APPLE__)
  // CLOCK_MONOTONIC_RAW == mach_absolute_time == std::steady_clock == Link's
  // host-time base. Plain CLOCK_MONOTONIC excludes sleep time on macOS.
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC_RAW, &ts);
  return (int64_t)ts.tv_sec * 1000000 + ts.tv_nsec / 1000;
#else
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (int64_t)ts.tv_sec * 1000000 + ts.tv_nsec / 1000;
#endif
}
