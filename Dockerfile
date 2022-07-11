FROM python:alpine
RUN pip install --no-cache-dir paho-mqtt prometheus-client
COPY main.py /
CMD ["python", "-u", "main.py"]
