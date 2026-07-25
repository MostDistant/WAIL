# DAW-less integration tests for the WAIL CLAP bridge plugins (ADR-0005).
# Hosts the built plugins via clap-trap and plays the WAIL app's role on the
# loopback IPC socket (see ipc_test_server.h), so the plugin ⇄ app contract
# (wail-app/ipc.go wire format) is exercised without a DAW or the Go app.

include(FetchContent)

# clap-trap provides the CLAP host (PluginLoader/TestHost). WASM plugin support
# and its own tests/CLI are not needed — keep the fetch lean.
set(CLAP_TRAP_WASM_SUPPORT OFF CACHE BOOL "" FORCE)
set(CLAP_TRAP_BUILD_TESTS OFF CACHE BOOL "" FORCE)
set(CLAP_TRAP_BUILD_EXAMPLES OFF CACHE BOOL "" FORCE)

FetchContent_Declare(
  clap-trap
  GIT_REPOSITORY https://github.com/dfl/clap-trap.git
  GIT_TAG v0.1.0
  GIT_SHALLOW TRUE
)
FetchContent_MakeAvailable(clap-trap)

# Shared settings for the two harness binaries (test runner + E2E chain driver).
function(add_wail_harness name)
  add_executable(${name} ${ARGN})
  target_include_directories(${name} PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}/..") # wail_ipc.h
  target_link_libraries(${name} PRIVATE clap-trap Threads::Threads linkbridge-link)
  target_compile_features(${name} PRIVATE cxx_std_20)
  if(WIN32)
    target_link_libraries(${name} PRIVATE ws2_32)
  endif()
  # Path to each built plugin binary (inside the .clap bundle on macOS — dlopen
  # resolves it directly; bundle layout itself is already verified by CI packaging).
  target_compile_definitions(${name} PRIVATE
    WAIL_SEND_CLAP_PATH="$<TARGET_FILE:wail-send>"
    WAIL_RECV_CLAP_PATH="$<TARGET_FILE:wail-recv>"
    WAIL_LINKBRIDGE_SPIKE_PATH="$<TARGET_FILE:linkbridge-spike>"
    WAIL_LINKBRIDGE_SEND_PATH="$<TARGET_FILE:linkbridge-send>"
  )
  add_dependencies(${name} wail-send wail-recv linkbridge-spike linkbridge-send)
endfunction()

add_wail_harness(wail-plugin-tests main.cpp test_send.cpp test_recv.cpp test_linkbridge.cpp)

# E2E chain driver for scripts/plugin-e2e.sh (not a ctest: needs two live apps).
add_wail_harness(wail-plugin-chain chain_main.cpp)

add_test(NAME wail-plugin-tests COMMAND wail-plugin-tests)
# The plugins' IPC threads poll/sleep (reconnect backoff, recv timeouts), so a
# failing test can take seconds to unwind; keep ctest patient.
set_tests_properties(wail-plugin-tests PROPERTIES TIMEOUT 120)
