# DAW-less integration tests for the WAIL Link Bridge plugins (ADR-0007).
# Hosts the built plugins via clap-trap alongside a second in-process Link peer,
# so the publish/subscribe and room-filter behaviour is exercised without a DAW.

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

# Shared settings for the harness binaries (test runner + mini-DAW driver).
function(add_wail_harness name)
  add_executable(${name} ${ARGN})
  target_include_directories(${name} PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}/..") # wail_thread.h
  target_link_libraries(${name} PRIVATE clap-trap Threads::Threads linkbridge-link)
  target_compile_features(${name} PRIVATE cxx_std_20)
  if(WIN32)
    target_link_libraries(${name} PRIVATE ws2_32)
  endif()
  # Path to each built plugin binary (inside the .clap bundle on macOS — dlopen
  # resolves it directly; bundle layout itself is already verified by CI packaging).
  target_compile_definitions(${name} PRIVATE
    WAIL_LINKBRIDGE_SPIKE_PATH="$<TARGET_FILE:linkbridge-spike>"
    WAIL_LINKBRIDGE_SEND_PATH="$<TARGET_FILE:wail-linkbridge-send>"
    WAIL_LINKBRIDGE_RECV_PATH="$<TARGET_FILE:wail-linkbridge-recv>"
  )
  add_dependencies(${name} linkbridge-spike wail-linkbridge-send wail-linkbridge-recv)
endfunction()

add_wail_harness(wail-plugin-tests main.cpp test_linkbridge.cpp)

# Mini-DAW system simulator (not a ctest: needs a live app + relay, see
# scripts/minidaw-e2e.sh).
add_wail_harness(minidaw minidaw_main.cpp)

add_test(NAME wail-plugin-tests COMMAND wail-plugin-tests)
# Link discovery and the real-time-paced pumps mean a failing test takes seconds
# to unwind, and each recv case waits out its own multi-second bound. A healthy
# run is ~13s; the budget is sized so failures still get to report.
set_tests_properties(wail-plugin-tests PROPERTIES TIMEOUT 240)
