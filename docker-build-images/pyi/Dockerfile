# Build against wheezy for broadest libc compatibility.
FROM ppc64le/python:2.7
MAINTAINER Shaun Crampton shaun@tigera.io

ARG UID
ARG GID

# Install Calico pre-reqs.
#RUN pip install pyinstaller
RUN git clone https://github.com/pyinstaller/pyinstaller.git
RUN cd pyinstaller && git checkout v3.2
RUN cd pyinstaller && cd bootloader && python2.7 ./waf distclean all --no-lsb 
RUN cd pyinstaller && python2.7 setup.py install

# Install the frozen build requirements.  Use `make update-frozen-reqs` to
# update these.
ADD python/requirements_frozen.txt python/requirements_frozen.txt
RUN pip install -U -r python/requirements_frozen.txt

# To run some commands (git, tox) as non-root user, we need to have a valid
# passwd/group file.
# use `--force` and `-o` since tests can run under root and command will fail with duplicate error
RUN groupadd --force --gid=$GID user && useradd -o --home=/ --gid=$GID --uid=$UID user

RUN mkdir /code
WORKDIR /code
