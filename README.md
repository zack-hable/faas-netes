# faas-netes
## Zack Hable and Michael Boby

# Install
Ensure you have faas-cli and kubectl available on your system.  Then run ```./my_install.sh```

# Building
To build the docker images run ```./my_build.sh```

# Additional Flags
To use the additional features provided by this you need to add them with the --label flag to the faas-cli
- cs2510.priority=<INTEGER> (integer value to represent how high the priority of the function's pods are.  Higher = more priority)
- cs2510.task.type=<IO|CPU> (IO for io bound tasks and CPU for cpu bound tasks, used for load balancing node assignment)
- cs2510.balancer=<NONE|LEAST_LOADED> (Which balancing to use for assigning functions to nodes.  NONE uses no node balancing and LEAST_LOADED assigns to least loaded nodes)
