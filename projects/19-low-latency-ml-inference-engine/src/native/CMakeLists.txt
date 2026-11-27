cmake_minimum_required(VERSION 3.18)
project(ml_inference_kernel LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(ml_inference_kernel SHARED
    inference_kernel.cpp
    inference_kernel.hpp
)
target_compile_options(ml_inference_kernel PRIVATE -O3 -Wall -Wextra -Wpedantic)

add_executable(bench_native
    bench_native.cpp
    inference_kernel.cpp
    inference_kernel.hpp
)
target_compile_options(bench_native PRIVATE -O3 -Wall -Wextra -Wpedantic)
find_package(Threads REQUIRED)
target_link_libraries(bench_native PRIVATE Threads::Threads)
