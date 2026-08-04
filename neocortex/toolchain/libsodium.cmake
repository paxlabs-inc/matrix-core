include(ExternalProject)

find_program(NEOCORTEX_MAKE_EXECUTABLE NAMES make gmake REQUIRED)

set(NEOCORTEX_SODIUM_PREFIX "${CMAKE_BINARY_DIR}/third_party/libsodium-install")
set(NEOCORTEX_SODIUM_LIBRARY "${NEOCORTEX_SODIUM_PREFIX}/lib/libsodium.a")
set(NEOCORTEX_SODIUM_CFLAGS "-O2 -fPIC")
set(NEOCORTEX_SODIUM_LDFLAGS "")
set(NEOCORTEX_SODIUM_HOST_ARGUMENT "")

if(CMAKE_C_COMPILER_TARGET)
  string(APPEND NEOCORTEX_SODIUM_CFLAGS " --target=${CMAKE_C_COMPILER_TARGET}")
  string(APPEND NEOCORTEX_SODIUM_CFLAGS " --sysroot=${CMAKE_SYSROOT}")
  string(APPEND NEOCORTEX_SODIUM_CFLAGS " -resource-dir=${NEOCORTEX_RESOURCE_DIR}")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " --target=${CMAKE_C_COMPILER_TARGET}")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " --sysroot=${CMAKE_SYSROOT}")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " -resource-dir=${NEOCORTEX_RESOURCE_DIR}")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " -fuse-ld=lld-18 --rtlib=compiler-rt")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " --unwindlib=libunwind")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " -L${NEOCORTEX_RUNTIME_DIR}")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " -L${NEOCORTEX_MULTIARCH_DIR}")
  if(NEOCORTEX_STATIC)
    string(APPEND NEOCORTEX_SODIUM_LDFLAGS " -static")
  endif()
  set(NEOCORTEX_SODIUM_HOST_ARGUMENT "--host=${CMAKE_C_COMPILER_TARGET}")
endif()

if(NEOCORTEX_SANITIZER STREQUAL "address-undefined")
  string(APPEND NEOCORTEX_SODIUM_CFLAGS " -fsanitize=address,undefined -fno-omit-frame-pointer")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " -fsanitize=address,undefined")
elseif(NEOCORTEX_SANITIZER STREQUAL "thread")
  string(APPEND NEOCORTEX_SODIUM_CFLAGS " -fsanitize=thread -fno-omit-frame-pointer")
  string(APPEND NEOCORTEX_SODIUM_LDFLAGS " -fsanitize=thread")
endif()

file(MAKE_DIRECTORY "${NEOCORTEX_SODIUM_PREFIX}/include")

ExternalProject_Add(neocortex_sodium_build
  SOURCE_DIR "${CMAKE_CURRENT_SOURCE_DIR}/third_party/libsodium"
  BINARY_DIR "${CMAKE_BINARY_DIR}/third_party/libsodium-build"
  CONFIGURE_COMMAND
    ${CMAKE_COMMAND} -E env
      "CC=${CMAKE_C_COMPILER}"
      "AR=${CMAKE_AR}"
      "RANLIB=${CMAKE_RANLIB}"
      "CFLAGS=${NEOCORTEX_SODIUM_CFLAGS}"
      "LDFLAGS=${NEOCORTEX_SODIUM_LDFLAGS}"
    <SOURCE_DIR>/configure
      --disable-shared
      --enable-static
      --enable-minimal
      --prefix=${NEOCORTEX_SODIUM_PREFIX}
      ${NEOCORTEX_SODIUM_HOST_ARGUMENT}
  BUILD_COMMAND ${NEOCORTEX_MAKE_EXECUTABLE} -j2
  INSTALL_COMMAND ${NEOCORTEX_MAKE_EXECUTABLE} install
  BUILD_BYPRODUCTS "${NEOCORTEX_SODIUM_LIBRARY}"
  LOG_CONFIGURE ON
  LOG_BUILD ON
  LOG_INSTALL ON
)

add_library(neocortex_sodium STATIC IMPORTED GLOBAL)
set_target_properties(neocortex_sodium PROPERTIES
  IMPORTED_LOCATION "${NEOCORTEX_SODIUM_LIBRARY}"
  INTERFACE_INCLUDE_DIRECTORIES "${NEOCORTEX_SODIUM_PREFIX}/include"
)
add_dependencies(neocortex_sodium neocortex_sodium_build)
