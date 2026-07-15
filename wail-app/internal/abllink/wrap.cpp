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
#endif

#include "abl_link.cpp"
