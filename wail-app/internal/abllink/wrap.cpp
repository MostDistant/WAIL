// Compilation unit for the vendored Ableton `abl_link` C wrapper.
//
// cgo compiles every .cpp in the package directory with the C++ toolchain, so we
// pull in the official wrapper (which instantiates the header-only Link SDK) here
// rather than copying it. Include paths and platform defines come from the cgo
// directives in abllink.go.
//
// Windows/MinGW: <windows.h> (dragged in transitively by asio) #defines `interface`
// as a macro, which collides with the parameter named `interface` in Link's
// link_audio/Channels.hpp. Pre-include the Windows headers and #undef the macro so
// the guarded re-includes downstream never redefine it. This is the C-side fix that
// moves out of the old abletonlink-go clone/patch and into our binding (migration
// plan precondition §9). Unverified in this environment (macOS only) — Windows CI
// must confirm.
#if defined(_WIN32)
#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#include <winsock2.h>
#include <windows.h>
#ifdef interface
#undef interface
#endif
#endif

#include "abl_link.cpp"
