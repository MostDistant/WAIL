cmake_minimum_required(VERSION 3.15)
project(wail_plugins C)

# Thin CLAP bridge plugins (ADR-0005). No external deps beyond the vendored CLAP
# headers, libpthread (POSIX) / ws2_32 (Windows), and the system sockets — the WAIL
# app owns all codec/relay logic, so these carry only raw PCM over loopback TCP.

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

add_wail_plugin(wail-send wail_send.c)
add_wail_plugin(wail-recv wail_recv.c)
