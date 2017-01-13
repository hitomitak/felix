FROM ppc64le/ubuntu:16.04
MAINTAINER Tom Denham <tom@projectcalico.org>

# Download and install glibc in one layer
RUN apt-get update -y
#RUN apt-get install -y libgcc 

RUN apt-get install -y iptables ipset iproute2 conntrack

ADD dist/calico-felix /code
WORKDIR /code

# Minimal dummy config
RUN mkdir /etc/calico && echo -e "[global]\nMetadataAddr = None\nLogFilePath = None\nLogSeverityFile = None" >/etc/calico/felix.cfg
RUN ln -s /code/calico-felix /usr/bin
RUN ln -s /code/calico-iptables-plugin /usr/bin

# Run felix by default
CMD ["calico-felix"]
