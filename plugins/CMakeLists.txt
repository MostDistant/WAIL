cmake_minimum_required(VERSION 3.15)
project(wail_plugins C CXX) # CXX: the Link Bridge plugins embed the Link SDK (ADR-0007)

# The Link Bridge CLAP plugins (ADR-0007) bring Link Audio into DAWs that don't
# speak it natively. Room intelligence (codec, intervals, relay, room clock) stays
# in the WAIL app; the plugins only publish/subscribe Link Audio channels.

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_C_VISIBILITY_PRESET hidden)

find_package(Threads REQUIRED)

set(CLAP_INCLUDE "${CMAKE_CURRENT_SOURCE_DIR}/../vendor/clap/include")
if(NOT EXISTS "${CLAP_INCLUDE}/clap/clap.h")
  message(FATAL_ERROR "CLAP headers not found at ${CLAP_INCLUDE}. Run: git submodule update --init vendor/clap")
endif()

function(add_wail_plugin name src)
  add_library(${name} MODULE "${src}")
  target_include_directories(${name} PRIVATE "${CLAP_INCLUDE}" "${CMAKE_CURRENT_SOURCE_DIR}")
  target_link_libraries(${name} PRIVATE Threads::Threads)
  if(UNIX AND NOT APPLE)
    target_link_libraries(${name} PRIVATE m)
  endif()
  if(WIN32)
    target_link_libraries(${name} PRIVATE ws2_32)
    set_target_properties(${name} PROPERTIES PREFIX "" SUFFIX ".clap")
  elseif(APPLE)
    set_target_properties(${name} PROPERTIES
      BUNDLE TRUE
      BUNDLE_EXTENSION "clap"
      OUTPUT_NAME "${name}"
      MACOSX_BUNDLE_INFO_PLIST "${CMAKE_CURRENT_SOURCE_DIR}/Info.plist.in"
      MACOSX_BUNDLE_BUNDLE_NAME "${name}"
      MACOSX_BUNDLE_GUI_IDENTIFIER "software.wail.${name}"
      MACOSX_BUNDLE_BUNDLE_VERSION "0.1.0"
      MACOSX_BUNDLE_SHORT_VERSION_STRING "0.1.0")
  else()
    set_target_properties(${name} PROPERTIES PREFIX "" SUFFIX ".clap")
  endif()
endfunction()

add_wail_plugin(transport-probe transport_probe.c) # dev diagnostic, not shipped

# Link Bridge plugins (ADR-0007): the Link SDK (C++17 + asio) compiled into the
# CLAP bundle itself — each plugin instance is its own LAN Link peer. Mirrors
# the include paths and platform defines of the app's cgo build (see
# wail-app/internal/abllink/abllink.go). Windows/MinGW additionally needs the
# `interface` param rename in the vendored Channels.hpp, same as the app build.
set(LINK_SDK "${CMAKE_CURRENT_SOURCE_DIR}/../vendor/link")
if(NOT EXISTS "${LINK_SDK}/extensions/abl_link/src/abl_link.cpp")
  message(FATAL_ERROR "Link SDK not found at ${LINK_SDK}. Run: git submodule update --init --recursive vendor/link")
endif()
# linkbridge-link: the C-facing Link session/audio API (linkbridge_link.h),
# shared by the bridge plugins AND the test harness (which subscribes to the
# send plugin's channel in-process). Link SDK/platform link deps are PUBLIC so
# consumers inherit them.
add_library(linkbridge-link STATIC linkbridge_link.cpp linkbridge_link.h)
target_include_directories(linkbridge-link PUBLIC "${CMAKE_CURRENT_SOURCE_DIR}" PRIVATE
  "${LINK_SDK}/extensions/abl_link/include"
  "${LINK_SDK}/extensions/abl_link/src"
  "${LINK_SDK}/include"
  "${LINK_SDK}/modules/asio-standalone/asio/include")
target_compile_features(linkbridge-link PUBLIC cxx_std_17)
set_target_properties(linkbridge-link PROPERTIES POSITION_INDEPENDENT_CODE ON)
target_link_libraries(linkbridge-link PUBLIC Threads::Threads)
if(WIN32)
  target_compile_definitions(linkbridge-link PRIVATE LINK_PLATFORM_WINDOWS=1 NOMINMAX)
  target_link_libraries(linkbridge-link PUBLIC ws2_32 winmm iphlpapi)
elseif(APPLE)
  target_compile_definitions(linkbridge-link PRIVATE LINK_PLATFORM_MACOSX=1 LINK_PLATFORM_UNIX=1)
else()
  target_compile_definitions(linkbridge-link PRIVATE LINK_PLATFORM_LINUX=1 LINK_PLATFORM_UNIX=1)
  target_link_libraries(linkbridge-link PUBLIC m atomic)
endif()

function(add_linkbridge_plugin name)
  add_library(${name} MODULE ${ARGN})
  target_include_directories(${name} PRIVATE "${CLAP_INCLUDE}" "${CMAKE_CURRENT_SOURCE_DIR}")
  set_target_properties(${name} PROPERTIES CXX_VISIBILITY_PRESET hidden VISIBILITY_INLINES_HIDDEN ON)
  target_link_libraries(${name} PRIVATE linkbridge-link)
  if(MSVC)
    # linkbridge_recv.c uses C11 atomics (wail_thread.h).
    target_compile_options(${name} PRIVATE /std:c11 /experimental:c11atomics)
  endif()
  if(WIN32)
    set_target_properties(${name} PROPERTIES PREFIX "" SUFFIX ".clap")
  elseif(APPLE)
    set_target_properties(${name} PROPERTIES
      BUNDLE TRUE
      BUNDLE_EXTENSION "clap"
      OUTPUT_NAME "${name}"
      MACOSX_BUNDLE_INFO_PLIST "${CMAKE_CURRENT_SOURCE_DIR}/Info.plist.in"
      MACOSX_BUNDLE_BUNDLE_NAME "${name}"
      MACOSX_BUNDLE_GUI_IDENTIFIER "software.wail.${name}"
      MACOSX_BUNDLE_BUNDLE_VERSION "0.1.0"
      MACOSX_BUNDLE_SHORT_VERSION_STRING "0.1.0")
  else()
    set_target_properties(${name} PROPERTIES PREFIX "" SUFFIX ".clap")
  endif()
endfunction()

add_linkbridge_plugin(linkbridge-spike linkbridge_spike.c) # dev spike (gate 1), not shipped
add_linkbridge_plugin(wail-linkbridge-send linkbridge_send.c)
add_linkbridge_plugin(wail-linkbridge-recv linkbridge_recv.c)

# DAW-less integration tests (clap-trap host + a second in-process Link peer).
# Off by default:
# enabling fetches clap-trap over the network at configure time.
option(WAIL_PLUGIN_TESTS "Build the CLAP plugin integration tests (fetches clap-trap)" OFF)
if(WAIL_PLUGIN_TESTS)
  enable_language(CXX) # tests only — the plugins themselves are pure C
  enable_testing()
  add_subdirectory(tests)
endif()
