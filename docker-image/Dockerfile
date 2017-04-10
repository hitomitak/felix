FROM ppc64le/ubuntu:16.04
MAINTAINER Tom Denham <tom@projectcalico.org>

# Download and install glibc in one layer
RUN apt-get update -y
#RUN apt-get install -y libgcc 

RUN apt-get install -y iptables ipset iproute2 conntrack

ADD felix.cfg /etc/calico/felix.cfg

# Put out binary in /code rather than directly in /usr/bin.  This allows the downstream builds
# to more easily extract the Felix build artefacts from the container.
RUN mkdir /code
ADD bin/calico-felix /code
WORKDIR /code
RUN ln -s /code/calico-felix /usr/bin

# Run felix by default
CMD ["calico-felix"]
