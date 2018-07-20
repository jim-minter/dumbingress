FROM scratch
COPY dumbingress .
ENTRYPOINT [ "/dumbingress" ]
