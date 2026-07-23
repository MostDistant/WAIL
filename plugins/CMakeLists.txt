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
  if(MSVC)
    # MSVC has no C11 atomics by default (even current VS2022): /std:c11 alone
    # makes vcruntime_c11_stdatomic.h #error "C atomic support is not enabled".
    # The plugins only use lock-free atomics (bool/int/u32/u64), which the
    # experimental flag covers.
    target_compile_options(${name} PRIVATE /std:c11 /experimental:c11atomics)
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

add_wail_plugin(wail-send wail_send.c)
add_wail_plugin(wail-recv wail_recv.c)

# State-extension roundtrip test: loads each built bundle and drives clap.state
# with a fake host (see test_state.c). wail-send also asserts its "Stream
# Index" param survives a save/load cycle.
if(NOT WIN32)
  enable_testing()
  add_executable(test_state test_state.c)
  target_include_directories(test_state PRIVATE "${CLAP_INCLUDE}")
  target_link_libraries(test_state PRIVATE ${CMAKE_DL_LIBS})
  add_dependencies(test_state wail-send wail-recv)
  add_test(NAME state_send COMMAND test_state $<TARGET_FILE:wail-send> software.wail.send stream-index)
  add_test(NAME state_recv COMMAND test_state $<TARGET_FILE:wail-recv> software.wail.recv)
endif()
