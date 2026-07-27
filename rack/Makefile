# If RACK_DIR is not defined when calling the Makefile, default to two directories above
RACK_DIR ?= ../..

# FLAGS will be passed to both the C and C++ compiler
FLAGS +=
CFLAGS +=
CXXFLAGS +=

# Careful about linking to shared libraries, since you can't assume much about the user's environment and library search path.
# Static libraries are fine, but they should be added to this plugin's build system.
LDFLAGS +=

# --- Ableton Link bridge -------------------------------------------------------
# Reuse WAIL's shared Link C wrapper instead of re-vendoring the SDK. It lives
# one level up in plugins/, and #includes vendor/link's abl_link.cpp (the whole
# header-only Ableton Link 4.0 + Link Audio SDK), so we compile ONE extra TU.
# VPATH lets us list it by basename so its object lands in build/ (not ../).
# Mirrors the include paths + C++17 in plugins/CMakeLists.txt (add_linkbridge_plugin).
VPATH += ../plugins
SOURCES += linkbridge_link.cpp

FLAGS += -I../plugins
FLAGS += -I../vendor/link/extensions/abl_link/include
FLAGS += -I../vendor/link/extensions/abl_link/src
FLAGS += -I../vendor/link/include
FLAGS += -I../vendor/link/modules/asio-standalone/asio/include
# -------------------------------------------------------------------------------

# Add .cpp files to the build
SOURCES += $(wildcard src/*.cpp)

# Add files to the ZIP package when running `make dist`
# The compiled plugin and "plugin.json" are automatically added.
DISTRIBUTABLES += res
DISTRIBUTABLES += $(wildcard LICENSE*)
DISTRIBUTABLES += $(wildcard presets)

# Include the Rack plugin Makefile framework
include $(RACK_DIR)/plugin.mk

# --- Link build settings that depend on the framework --------------------------
# Appended AFTER plugin.mk so (a) ARCH_OS/ARCH_CPU from arch.mk are defined and
# (b) this -std lands last (compile.mk sets -std=c++11, then folds FLAGS in),
# so C++17 wins for the Link TU. C++-only via CXXFLAGS (the plugin has no .c).
CXXFLAGS += -std=c++17

ifeq ($(ARCH_OS), mac)
	FLAGS += -DLINK_PLATFORM_MACOSX=1 -DLINK_PLATFORM_UNIX=1
endif
ifeq ($(ARCH_OS), lin)
	FLAGS += -DLINK_PLATFORM_LINUX=1 -DLINK_PLATFORM_UNIX=1
	LDFLAGS += -latomic
endif
ifeq ($(ARCH_OS), win)
	# NOTE: MinGW also needs the vendored Channels.hpp `interface` param rename
	# (see WAIL DEVELOPMENT.md / release.yml). Windows is untested for this plugin.
	FLAGS += -DLINK_PLATFORM_WINDOWS=1 -DNOMINMAX
	LDFLAGS += -lws2_32 -lwinmm -liphlpapi
endif
# -------------------------------------------------------------------------------
