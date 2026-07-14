// Compilation unit for the vendored Ableton `abl_link` C wrapper.
//
// cgo compiles every .cpp in the package directory with the C++ toolchain, so we
// pull in the official wrapper (which instantiates the header-only Link SDK) here
// rather than copying it. Include paths and platform defines come from the cgo
// directives in abllink.go.
//
// Windows/MinGW: <combaseapi.h> #defines the COM macro `interface` (as `struct`),
// which collides with the parameter named `interface` in Link's
// link_audio/Channels.hpp. We pull the Windows headers in first and #undef the
// macro; include guards keep it gone for the transitive re-includes from asio.
//
// Crucially we do NOT define WIN32_LEAN_AND_MEAN — that would make <windows.h>
// skip <combaseapi.h>, so the macro would be defined only later (by asio) and our
// #undef would be a no-op (the bug that broke the first Windows CI build). asio
// still needs <winsock2.h> before <windows.h>. This is the C-side MinGW fix
// living in our binding (migration plan precondition §9), replacing the old
// sed-patch of the vendored header.
#if defined(_WIN32)
#define NOMINMAX
#include <winsock2.h>
#include <windows.h>
#ifdef interface
#undef interface
#endif
#endif

#include "abl_link.cpp"
