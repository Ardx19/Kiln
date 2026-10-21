# Smallest project that makes CMake emit dyndep: two C++20 modules, one
# importing the other, and an executable importing both.
cmake_minimum_required(VERSION 3.28)
project(kiln_cxx_modules LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

add_library(shapes)
target_sources(shapes PUBLIC FILE_SET CXX_MODULES FILES area.cppm shapes.cppm)

add_executable(main main.cpp)
target_link_libraries(main PRIVATE shapes)
