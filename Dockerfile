FROM python:alpine
RUN pip install --no-cache-dir paho-mqtt prometheus-client pyaml
COPY main.py /
ENTRYPOINT ["python", "-u", "main.py"]
